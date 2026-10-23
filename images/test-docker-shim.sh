#!/usr/bin/env bash
# Unit tests for assets/docker-shim.sh. Linux-only (like the shim: it reads
# /proc/<pid>/comm) and needs bash, coreutils (`timeout`, `sleep`, `env`),
# grep and diff; no docker daemon is required. A fake docker records its
# argv, and a bash script named Runner.Worker stands in for the runner so
# /proc/<pid>/comm reads "Runner.Worker".
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SHIM="$HERE/assets/docker-shim.sh"
TMP="$(mktemp -d)"
trap 'kill "${WORKER_PID:-}" 2>/dev/null || true; rm -rf "$TMP"' EXIT

# Fake docker: one argv element per line, its environment to a second file,
# then exit with $FAKE_DOCKER_EXIT.
cat > "$TMP/docker" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$FAKE_DOCKER_ARGV"
env > "$FAKE_DOCKER_ENV"
exit "${FAKE_DOCKER_EXIT:-0}"
FAKE
chmod +x "$TMP/docker"
export FIRERUNNER_SHIM_REAL_DOCKER="$TMP/docker"
export FAKE_DOCKER_ARGV="$TMP/argv"
export FAKE_DOCKER_ENV="$TMP/env"

# A process whose comm is "Runner.Worker": the kernel names a process after
# the file it executes, so a script of that name will do. Not a renamed copy
# of `sleep`: a multi-call coreutils (uutils, on newer runner images) refuses
# to run under a name that is not one of its utilities. The script waits on a
# child sleep and takes it down when killed.
cat > "$TMP/Runner.Worker" <<'WORKER'
#!/bin/bash
sleep 300 &
trap 'kill $! 2>/dev/null; exit 0' TERM
wait
WORKER
chmod +x "$TMP/Runner.Worker"
"$TMP/Runner.Worker" &
WORKER_PID=$!
for _ in $(seq 1 50); do
	[ "$(cat /proc/$WORKER_PID/comm 2>/dev/null)" = "Runner.Worker" ] && break
	sleep 0.05
done
[ "$(cat /proc/$WORKER_PID/comm)" = "Runner.Worker" ] || { echo "could not stage a Runner.Worker parent"; exit 1; }

fails=0
check() { # name expected-argv-file
	if diff -u "$2" "$FAKE_DOCKER_ARGV" >"$TMP/diff"; then
		echo "ok   $1"
	else
		echo "FAIL $1"; cat "$TMP/diff"; fails=$((fails+1))
	fi
}
expect() { printf '%s\n' "$@" > "$TMP/want"; }
run_shim() { : > "$FAKE_DOCKER_ARGV"; env "$@"; }

URL="http://172.19.3.1:8099/"

# 1. Runner.Worker + create + URL set -> injected right after `create`, original
#    options preserved (including an argument containing spaces).
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	"$SHIM" create --name abc -e "FOO=bar baz" --entrypoint tail img:1 -f /dev/null
expect create -e "ACTIONS_RESULTS_URL=$URL" -e "ACTIONS_CACHE_SERVICE_V2=true" --name abc -e "FOO=bar baz" --entrypoint tail img:1 -f /dev/null
check "injects into runner's docker create" "$TMP/want"

# 2. An existing ACTIONS_CACHE_SERVICE_V2 value is forwarded, not overwritten.
run_shim ACTIONS_RESULTS_URL="$URL" ACTIONS_CACHE_SERVICE_V2=false FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	"$SHIM" create img
expect create -e "ACTIONS_RESULTS_URL=$URL" -e "ACTIONS_CACHE_SERVICE_V2=false" img
check "forwards existing ACTIONS_CACHE_SERVICE_V2" "$TMP/want"

# 3. No URL (non-redirect golden, or cache not configured) -> pure passthrough.
run_shim -u ACTIONS_RESULTS_URL FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	"$SHIM" create --name abc img
expect create --name abc img
check "passthrough when ACTIONS_RESULTS_URL unset" "$TMP/want"

# 4. Other subcommands are never touched, even from the runner with a URL.
for sub in exec run ps pull start; do
	run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" \
		"$SHIM" "$sub" -e ACTIONS_RESULTS_ORL abc sh -c 'echo hi'
	expect "$sub" -e ACTIONS_RESULTS_ORL abc sh -c 'echo hi'
	check "passthrough for docker $sub" "$TMP/want"
done

# 5. A job's own `docker create` (parent is not Runner.Worker) is untouched.
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$$" \
	"$SHIM" create --name mine img
expect create --name mine img
check "passthrough when caller is not Runner.Worker" "$TMP/want"

# 6. A bogus parent pid (comm unreadable) is treated as not-the-runner.
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="999999999" \
	"$SHIM" create img
expect create img
check "passthrough when parent comm unreadable" "$TMP/want"

# 7. The real docker's exit code propagates.
set +e
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" FAKE_DOCKER_EXIT=3 "$SHIM" create img
rc=$?
set -e
if [ "$rc" = 3 ]; then echo "ok   propagates exit code"; else echo "FAIL exit code = $rc, want 3"; fails=$((fails+1)); fi

# 8. Missing real docker -> 127 with a message, no crash.
set +e
out="$(FIRERUNNER_SHIM_REAL_DOCKER="$TMP/does-not-exist" "$SHIM" ps 2>&1)"; rc=$?
set -e
if [ "$rc" = 127 ] && [[ "$out" == *"real docker binary not found"* ]]; then
	echo "ok   missing real docker -> 127"
else
	echo "FAIL missing real docker: rc=$rc out=$out"; fails=$((fails+1))
fi

# 9. Unset override falls back to the fixed search list and never picks itself.
#    With PATH holding only a copy of the shim, `docker` resolves to that copy,
#    which must hand off to a real binary outside its own directory — or fail
#    cleanly with 127 when the host has none. `--version` never touches a
#    daemon or the network, so this is deterministic on any host; a recursion
#    would show up as a hang (bounded by timeout) or a non-0/127 exit.
mkdir -p "$TMP/lonely"; cp "$SHIM" "$TMP/lonely/docker"
set +e
BASH_BIN="$(command -v bash)"
out="$(timeout 10 env -u FIRERUNNER_SHIM_REAL_DOCKER -u ACTIONS_RESULTS_URL PATH="$TMP/lonely" \
	"$BASH_BIN" -c 'exec docker --version' 2>&1)"; rc=$?
set -e
case "$rc" in
	0)   if [[ "$out" == Docker\ version* ]]; then echo "ok   no recursion (handed off to the host's real docker)"; else echo "FAIL unexpected --version output: $out"; fails=$((fails+1)); fi;;
	127) if [[ "$out" == *"real docker binary not found"* ]]; then echo "ok   no recursion (no real docker on this host, rc=127)"; else echo "FAIL rc=127 but shim message missing: $out"; fails=$((fails+1)); fi;;
	124) echo "FAIL recursion guard: shim hung (timeout)"; fails=$((fails+1));;
	*)   echo "FAIL recursion guard: rc=$rc out=$out"; fails=$((fails+1));;
esac

# 9b. Test hooks are ignored when Runner.Worker itself runs the shim. A bash
#     copy named Runner.Worker execs the shim with FIRERUNNER_SHIM_REAL_DOCKER
#     pointing at a decoy: the decoy must not run, and the call must fall back
#     to the fixed lookup (real docker's --version, or 127 with no docker).
cp "$BASH_BIN" "$TMP/Runner.Worker.bash"; mv "$TMP/Runner.Worker.bash" "$TMP/rw/Runner.Worker" 2>/dev/null || { mkdir -p "$TMP/rw"; mv "$TMP/Runner.Worker.bash" "$TMP/rw/Runner.Worker"; }
cat > "$TMP/decoy" <<'DECOY'
#!/usr/bin/env bash
echo DECOY-RAN > "$DECOY_MARKER"; echo "Docker version decoy"; exit 0
DECOY
chmod +x "$TMP/decoy"; rm -f "$TMP/decoy.marker"
set +e
out="$(DECOY_MARKER="$TMP/decoy.marker" FIRERUNNER_SHIM_REAL_DOCKER="$TMP/decoy" FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	timeout 10 "$TMP/rw/Runner.Worker" -c '"$0" --version; exit $?' "$SHIM" 2>&1)"; rc=$?
# (the trailing `exit $?` stops bash from tail-call exec'ing the shim, so its
# parent really is the process named Runner.Worker)
set -e
if [ -e "$TMP/decoy.marker" ]; then
	echo "FAIL Runner.Worker-run shim honoured FIRERUNNER_SHIM_REAL_DOCKER"; fails=$((fails+1))
elif [ "$rc" = 0 ] && [[ "$out" == Docker\ version* ]] && [[ "$out" != *decoy* ]]; then
	echo "ok   test hooks ignored under Runner.Worker (real docker used)"
elif [ "$rc" = 127 ] && [[ "$out" == *"real docker binary not found"* ]]; then
	echo "ok   test hooks ignored under Runner.Worker (no real docker, rc=127)"
else
	echo "FAIL hooks-under-runner: rc=$rc out=$out"; fails=$((fails+1))
fi

# 10. The environment reaches the real docker verbatim — including names that
#     are not valid shell identifiers. The runner passes step inputs as
#     `-e INPUT_INCLUDE-HIDDEN-FILES` and relies on the docker CLI reading the
#     value from its own environment; a /bin/sh (dash) shim strips such names
#     and every hyphenated action input arrives empty.
run_shim ACTIONS_RESULTS_URL="$URL" FIRERUNNER_SHIM_PPID="$WORKER_PID" \
	'INPUT_INCLUDE-HIDDEN-FILES=false' 'INPUT_RETENTION-DAYS=14' 'INPUT_IF-NO-FILES-FOUND=warn' \
	"$SHIM" exec -e INPUT_INCLUDE-HIDDEN-FILES -e INPUT_RETENTION-DAYS abc node /index.js
envfail=0
for kv in 'INPUT_INCLUDE-HIDDEN-FILES=false' 'INPUT_RETENTION-DAYS=14' 'INPUT_IF-NO-FILES-FOUND=warn' "ACTIONS_RESULTS_URL=$URL"; do
	grep -qxF "$kv" "$FAKE_DOCKER_ENV" || { echo "     missing from docker env: $kv"; envfail=1; }
done
if [ "$envfail" = 0 ]; then echo "ok   hyphenated env names survive the shim"; else echo "FAIL hyphenated env names dropped"; fails=$((fails+1)); fi

# 11. The shim is bash, not sh: /bin/sh is dash on the golden and would fail
#     test 10 (see the header comment in the shim).
if head -1 "$SHIM" | grep -q '^#!/bin/bash'; then echo "ok   shim runs under bash"; else echo "FAIL shim shebang is not /bin/bash: $(head -1 "$SHIM")"; fails=$((fails+1)); fi

[ "$fails" = 0 ] && echo "all docker-shim tests passed" || { echo "$fails test(s) failed"; exit 1; }
