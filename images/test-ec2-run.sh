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

cat > "$TMP/runner/run.sh" <<'FAKE'
#!/bin/sh
printf '%s\n' "$@" > "$STATE/runner-argv"
FAKE
chmod +x "$TMP/runner/run.sh"

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
# powered_off <mode>: the script powered off exactly once, in that mode.
powered_off() { [ "$(cat "$STATE/poweroff" 2>/dev/null)" = "$1" ]; }

# run: run the script against the $STATE prepared by the caller, killing it
# if it hangs (no coreutils timeout on macOS).
run() {
	PATH="$TMP/bin:$PATH" \
		FIRERUNNER_IMDS=imds.test \
		FIRERUNNER_RUNNER_DIR="$TMP/runner" \
		FIRERUNNER_POWEROFF="$TMP/bin/fake-poweroff clean" \
		FIRERUNNER_POWEROFF_FAST="$TMP/bin/fake-poweroff fast" \
		FIRERUNNER_RETRY_SLEEP=0 \
		FIRERUNNER_JIT_WAIT="${JIT_WAIT:-30}" \
		sh "$SCRIPT" 2> "$STATE/stderr" &
	local pid=$!
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
run
expect "runner gets the fetched config" "argv=$(cat "$STATE/runner-argv" 2>/dev/null)" \
	[ "$(cat "$STATE/runner-argv" 2>/dev/null)" = "$(printf -- '--jitconfig\neyJqaXQiOiJ4In0=')" ]
expect "powers off fast after the job" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast
expect "retries through 000 and 403" "jit requests=$(jit_requests)" [ "$(jit_requests)" = 4 ]
expect "exits 0" "$(cat "$STATE/exit" 2>/dev/null)" lacks exit

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
printf '#!/bin/sh\nexit 3\n' > "$TMP/runner/run.sh"
run
expect "runner failure: powers off fast" "poweroff=$(cat "$STATE/poweroff" 2>/dev/null)" powered_off fast

for st in "$TMP"/state-*; do
	expect "$(basename "$st"): finished" "hung" [ ! -f "$st/hung" ]
done

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
