#!/usr/bin/env bash
# Unit tests for assets/firerunner-ec2-run.sh. Needs bash, sh, sed and
# coreutils; no network. A fake curl plays IMDS and the control VM's /jit
# endpoint, a fake run.sh records its argv, and a fake poweroff records that
# the instance would have shut down.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/assets/firerunner-ec2-run.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin" "$TMP/runner"

# Fake curl. State lives in $STATE:
#   token-fails  how many token requests fail before IMDS answers
#   userdata     the user-data body (absent = IMDS returns 404)
#   jit-codes    one HTTP status per /jit request, in order; the last repeats
#   jit-body     the body served with a 200
#   reports      written: the query of each POST to /jit/report, one per line
cat > "$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
out="" write="" url=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift ;;
	-w) write="$2"; shift ;;
	-m|-X|-H) shift ;;
	http*) url="$1" ;;
	esac
	shift
done
echo "$url" >> "$STATE/requests"
case "$url" in
*/latest/api/token)
	n=$(cat "$STATE/token-fails" 2>/dev/null || echo 0)
	if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$STATE/token-fails"; exit 7; fi
	echo "imds-token"
	;;
*/latest/user-data)
	[ -f "$STATE/userdata" ] || exit 22
	cat "$STATE/userdata"
	;;
*/jit/report\?*)
	echo "${url#*\?}" >> "$STATE/reports"
	if [ -n "$write" ]; then printf 204; fi
	;;
*/jit)
	codes=($(cat "$STATE/jit-codes"))
	code=${codes[0]}
	[ ${#codes[@]} -gt 1 ] && echo "${codes[@]:1}" > "$STATE/jit-codes"
	if [ "$code" = 000 ]; then printf 000; exit 7; fi
	if [ "$code" = 200 ]; then cat "$STATE/jit-body" > "$out"; else : > "$out"; fi
	printf '%s' "$code"
	;;
*) exit 6 ;;
esac
FAKE
chmod +x "$TMP/bin/curl"

# runner BODY: the fake run.sh records its argv, then runs BODY.
runner() {
	{
		cat <<'FAKE'
#!/bin/sh
printf '%s\n' "$@" > "$STATE/runner-argv"
FAKE
		printf '%s\n' "$1"
	} > "$TMP/runner/run.sh"
	chmod +x "$TMP/runner/run.sh"
}
runner :

# Records which poweroff ran: "clean" or "fast" (its only argument).
cat > "$TMP/bin/fake-poweroff" <<'FAKE'
#!/bin/sh
echo "$1" >> "$STATE/poweroff"
FAKE
chmod +x "$TMP/bin/fake-poweroff"

fails=0
# expect <name> <detail> <command...>: pass when the command succeeds.
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then
		echo "ok   $name"
	else
		echo "FAIL $name: $detail"
		fails=$((fails + 1))
	fi
}
has() { [ -f "$STATE/$1" ]; }
lacks() { [ ! -f "$STATE/$1" ]; }
jit_requests() { grep -c '/jit$' "$STATE/requests" 2>/dev/null || true; }
# reported <query>: the script posted this report to /jit/report.
reported() { grep -qx "$1" "$STATE/reports" 2>/dev/null; }
# powered_off <mode>: the script powered off exactly once, in that mode.
powered_off() { [ "$(cat "$STATE/poweroff" 2>/dev/null)" = "$1" ]; }

# script: the boot script under test, with the fakes and SIGPIPE ignored, as
# systemd starts it; it replaces the calling (sub)shell, so a backgrounded
# call's $! is the script itself.
script() {
	# shellcheck disable=SC2016 # expanded by the inner shell
	exec env PATH="${EXTRA_PATH:+$EXTRA_PATH:}$TMP/bin:$PATH" \
		FIRERUNNER_IMDS=imds.test \
		FIRERUNNER_RUNNER_DIR="${RUNNER_DIR:-$TMP/runner}" \
		FIRERUNNER_POWEROFF="$TMP/bin/fake-poweroff clean" \
		FIRERUNNER_POWEROFF_FAST="$TMP/bin/fake-poweroff fast" \
		FIRERUNNER_RETRY_SLEEP=0 \
		FIRERUNNER_DRAIN_TICKS="${DRAIN_TICKS:-50}" \
		FIRERUNNER_JIT_WAIT="${JIT_WAIT:-30}" \
		"${SCRIPT_SH:-sh}" -c 'trap "" PIPE; exec "$0" "$1"' "${SCRIPT_SH:-sh}" "$SCRIPT"
}

# run: run the script against the $STATE prepared by the caller, killing it
# if it hangs (no coreutils timeout on macOS). With BROKEN_STREAM set, its
# stdout and stderr are a pipe nobody reads, as when the journal stream
# breaks.
run() {
	if [ -n "${BROKEN_STREAM:-}" ]; then
		(script 2>&1 | true) &
	else
		script > "$STATE/stdout" 2> "$STATE/stderr" &
	fi
	local pid=$!
	if [ -n "${TERM_WHEN:-}" ]; then
		# Signal the script once $STATE/$TERM_WHEN exists, as systemd does on
		# a shutdown or a spot interruption.
		(until [ -f "$STATE/$TERM_WHEN" ]; do sleep 0.05; done; kill -TERM "$pid") &
	fi
	(sleep 20 && kill "$pid" 2>/dev/null && touch "$STATE/hung") &
	local watchdog=$!
	local rc=0
	wait "$pid" || rc=$?
	kill "$watchdog" 2>/dev/null || true
	wait "$watchdog" 2>/dev/null || true
	[ "$rc" -eq 0 ] || echo "exit $rc" > "$STATE/exit"
}

fresh() {
	STATE="$TMP/state-$1"
	mkdir -p "$STATE"
	export STATE
}

# 1. Happy path: IMDS is late, the address is not registered yet (403), then
#    the config arrives and the runner gets it.
fresh happy
echo 2 > "$STATE/token-fails"
printf '#!/bin/sh\nFIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit\n' > "$STATE/userdata"
echo "000 403 403 200" > "$STATE/jit-codes"
printf 'eyJqaXQiOiJ4In0=' > "$STATE/jit-body"
runner 'echo "Listening for Jobs"; echo "Running job: build"; echo "Job build completed"; echo "Exiting runner..."'
run
expect "runner gets the fetched config" "argv=$(cat "$STATE/runner-argv" 2>/dev/null)" \
	[ "$(cat "$STATE/runner-argv" 2>/dev/null)" = "$(printf -- '--jitconfig\neyJqaXQiOiJ4In0=')" ]
expect "powers off fast after the job" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
expect "retries through 000 and 403" "jit requests=$(jit_requests)" [ "$(jit_requests)" = 4 ]
expect "exits 0" "$(cat "$STATE/exit" 2>/dev/null)" lacks exit
expect "reports listening" "$(cat "$STATE/reports" 2>/dev/null)" reported phase=listening
expect "reports the job start" "$(cat "$STATE/reports" 2>/dev/null)" reported phase=job
expect "reports a zero exit" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=0'
expect "passes the runner's output on" "$(cat "$STATE/stdout")" grep -qx "Job build completed" "$STATE/stdout"
expect "reports the phases before the exit" "$(cat "$STATE/reports" 2>/dev/null)" \
	[ "$(tr '\n' ' ' < "$STATE/reports")" = "phase=listening phase=job phase=exited&rc=0 " ]
runner :

# 2. No endpoint in user-data (a warm-up or image-build boot): power off
#    without contacting any endpoint or starting the runner.
fresh no-url
run
expect "no user-data: runner not started" "runner ran" lacks runner-argv
expect "no user-data: powers off cleanly (the disk boots again)" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off clean
expect "no user-data: no jit request" "jit requests=$(jit_requests)" [ "$(jit_requests)" = 0 ]
expect "no user-data: warms the runner's files" "$(cat "$STATE/stderr")" grep -q "warm-up boot" "$STATE/stderr"

# 3. 404: the config was already claimed or expired. Give up at once rather
#    than retrying forever.
fresh claimed
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "404 200" > "$STATE/jit-codes"
printf 'late' > "$STATE/jit-body"
run
expect "404: runner not started" "runner ran" lacks runner-argv
expect "404: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
expect "404: no retry" "jit requests=$(jit_requests)" [ "$(jit_requests)" = 1 ]

# 4. The config never arrives: stop after FIRERUNNER_JIT_WAIT.
fresh never
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "403" > "$STATE/jit-codes"
JIT_WAIT=1 run
expect "timeout: runner not started" "runner ran" lacks runner-argv
expect "timeout: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
expect "timeout: says why" "$(cat "$STATE/stderr")" grep -q "no JIT config after 1s" "$STATE/stderr"

# 5. Only the FIRERUNNER_JIT_URL line is used, whatever else user-data holds.
fresh noisy
printf 'OTHER=1\nFIRERUNNER_JIT_URL=http://10.0.0.7:9000/jit\nFIRERUNNER_JIT_URL=http://evil/jit\n' > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
printf 'cfg' > "$STATE/jit-body"
run
expect "uses the first FIRERUNNER_JIT_URL" "$(cat "$STATE/requests")" \
	grep -qx 'http://10.0.0.7:9000/jit' "$STATE/requests"
expect "ignores later FIRERUNNER_JIT_URL lines" "$(cat "$STATE/requests")" \
	[ "$(grep -c evil "$STATE/requests" || true)" = 0 ]

# 6. A failing runner still powers the instance off.
fresh runner-fails
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
printf 'cfg' > "$STATE/jit-body"
runner 'echo "Listening for Jobs"; exit 3'
run
expect "runner failure: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
expect "runner failure: reports its status" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=3'
expect "runner failure: says so" "$(cat "$STATE/stderr")" grep -q "runner exited with status 3" "$STATE/stderr"
runner :

# 7. No runner to start: report it as a failure, so firerunner backs off.
fresh no-runner
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
printf 'cfg' > "$STATE/jit-body"
RUNNER_DIR="$TMP/missing" run
expect "missing runner: reports status 127" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=127'
expect "missing runner: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
expect "missing runner: no other report" "$(cat "$STATE/reports" 2>/dev/null)" [ "$(wc -l < "$STATE/reports")" -eq 1 ]

fresh no-runner-broken-stream
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
printf 'cfg' > "$STATE/jit-body"
RUNNER_DIR="$TMP/missing" BROKEN_STREAM=1 run
expect "missing runner, broken stream: reports status 127" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=127'
expect "missing runner, broken stream: powers off" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast

# 8. Something the job left behind holds the runner's output open: the script
#    still finishes with the runner, not with it: at once when run.sh says
#    it is exiting, and within the drain bound when it does not.
for marker in yes no; do
	fresh "lingering-$marker"
	echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
	echo "200" > "$STATE/jit-codes"
	printf 'cfg' > "$STATE/jit-body"
	if [ "$marker" = yes ]; then
		runner 'sleep 8 & echo "Listening for Jobs"; echo "Exiting runner..."'
	else
		runner 'sleep 8 & echo "Listening for Jobs"'
	fi
	# With the marker, the drain bound (3 s) is never reached.
	if [ "$marker" = yes ]; then ticks=30 limit=2; else ticks=10 limit=4; fi
	began=$(date +%s)
	DRAIN_TICKS=$ticks run
	expect "lingering output holder ($marker marker): finishes with the runner" "took $(($(date +%s) - began))s" [ $(($(date +%s) - began)) -lt $limit ]
	expect "lingering output holder ($marker marker): reports the exit" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=0'
done
runner :

# 8b. Upstream's run.sh exits 0 for most fatal listener statuses; the
#     listener's status comes from run-helper.sh's line for it.
while read -r want line; do
	fresh "listener-$want"
	echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
	echo "200" > "$STATE/jit-codes"
	printf 'cfg' > "$STATE/jit-body"
	runner "echo '$line'; echo 'Exiting runner...'; exit 0"
	run
	expect "listener status $want from \"$line\"" "$(cat "$STATE/reports" 2>/dev/null)" reported "phase=exited&rc=$want"
done <<'LINES'
1 Runner listener exit with terminated error, stop the service, no retry needed.
5 Runner listener exit with Session Conflict error, stop the service, no retry needed.
42 Exiting with unknown error code: 42
0 Runner listener exit with 0 return code, stop the service, no retry needed.
0 Exiting with unknown error code: x
LINES
runner :

# 9. The config was served but its response lost, so the retry gets 404: the
#    runner never ran, so report a failure (firerunner accepts it only if it
#    did serve this address).
fresh lost-response
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "000 404" > "$STATE/jit-codes"
run
expect "lost response: reports a failure" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=1'
expect "lost response: runner not started" "runner ran" lacks runner-argv

# 10. An empty config: the runner cannot start; report a failure.
fresh empty-config
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
: > "$STATE/jit-body"
run
expect "empty config: reports a failure" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=1'
expect "empty config: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast

# 11. run.sh exists but is not executable: status 127.
fresh not-executable
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
printf 'cfg' > "$STATE/jit-body"
chmod -x "$TMP/runner/run.sh"
run
chmod +x "$TMP/runner/run.sh"
expect "not executable: reports status 127" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=127'
expect "not executable: runner not started" "runner ran" lacks runner-argv

# 12. A step between the fetch and the runner fails under set -e: the exit
#     trap still reports the failure.
fresh mkfifo-fails
echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
echo "200" > "$STATE/jit-codes"
printf 'cfg' > "$STATE/jit-body"
mkdir -p "$TMP/broken"
printf '#!/bin/sh\nexit 1\n' > "$TMP/broken/mkfifo"
chmod +x "$TMP/broken/mkfifo"
EXTRA_PATH="$TMP/broken" run
expect "failed setup: reports a failure" "$(cat "$STATE/reports" 2>/dev/null)" reported 'phase=exited&rc=1'
expect "failed setup: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast

# 13. A signal (shutdown, spot interruption, terminate) is the instance going
#     away, not the runner failing: no exit report, and no second poweroff.
#     Under bash too: dash never runs the EXIT trap on a signal, bash does.
for sh in sh bash; do
	fresh "signalled-$sh"
	echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
	echo "200" > "$STATE/jit-codes"
	printf 'cfg' > "$STATE/jit-body"
	# shellcheck disable=SC2016 # expanded by the fake run.sh
	runner 'touch "$STATE/running"; echo "Listening for Jobs"; sleep 1'
	SCRIPT_SH=$sh TERM_WHEN=running run
	runner :
	expect "signal ($sh): no exit report" "$(cat "$STATE/reports" 2>/dev/null)" [ "$(grep -c '^phase=exited' "$STATE/reports" 2>/dev/null || true)" = 0 ]
	expect "signal ($sh): no poweroff" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" lacks poweroff
done

# 14. The journal stream breaks mid-job (the script's stdout and stderr lose
#     their reader): the runner keeps running, and its exit is still
#     reported, whatever it is, before the instance powers off.
for want in 0 3; do
	fresh "broken-stream-$want"
	echo "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit" > "$STATE/userdata"
	echo "200" > "$STATE/jit-codes"
	printf 'cfg' > "$STATE/jit-body"
	# shellcheck disable=SC2016 # expanded by the fake run.sh
	# A runner that fails on a write error, as the real one may.
	runner 'set -e; echo "Listening for Jobs"; sleep 0.3; i=0; while [ $i -lt 200 ]; do echo "line $i"; i=$((i + 1)); done; touch "$STATE/runner-done"; exit '"$want"
	BROKEN_STREAM=1 run
	runner :
	expect "broken stream, status $want: the runner finishes" "runner did not finish" has runner-done
	expect "broken stream, status $want: reports it" "$(cat "$STATE/reports" 2>/dev/null)" reported "phase=exited&rc=$want"
	expect "broken stream, status $want: powers off" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
done

for st in "$TMP"/state-*; do
	expect "$(basename "$st"): finished" "hung" [ ! -f "$st/hung" ]
done

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
