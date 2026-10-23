#!/usr/bin/env bash
# Tests for scripts/pr-wait.sh against a fake gh. Needs bash and jq; no
# network. The fake serves the PR from $STATE/view.json, one checks snapshot
# per `gh pr checks` call ($STATE/checks-1.json, -2, …; the last repeats),
# failed logs from $STATE/log-JOB.txt and GraphQL from $STATE/graphql.json,
# and records every call in $STATE/calls.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/pr-wait.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$STATE/calls"
jqf="" required=0 job="" args=()
while [ $# -gt 0 ]; do
	case "$1" in
	--jq) jqf="$2"; shift ;;
	--required) required=1 ;;
	--job) job="$2"; shift ;;
	--json|--repo|-F|-f) shift ;;
	*) args+=("$1") ;;
	esac
	shift
done
set -- "${args[@]}"
case "$1 $2" in
"pr view")
	[ -f "$STATE/view.json" ] || { echo "no pull requests found for branch \"$3\"" >&2; exit 1; }
	json="$(cat "$STATE/view.json")"
	;;
"pr checks")
	if [ "$required" = 1 ]; then
		[ -f "$STATE/required.json" ] || { echo "no required checks reported on the 'b' branch" >&2; exit 1; }
		json="$(cat "$STATE/required.json")"
	else
		n=$(($(cat "$STATE/polls" 2>/dev/null || echo 0) + 1))
		echo "$n" >"$STATE/polls"
		while [ "$n" -gt 1 ] && [ ! -f "$STATE/checks-$n.json" ]; do n=$((n - 1)); done
		[ -f "$STATE/checks-$n.json" ] || { echo "no checks reported on the 'b' branch" >&2; exit 1; }
		json="$(cat "$STATE/checks-$n.json")"
		echo "$json"
		# Like gh: 1 when a check failed, 8 while any is pending.
		if jq -e 'any(.[]; .bucket == "fail")' >/dev/null <<<"$json"; then exit 1; fi
		if jq -e 'any(.[]; .bucket == "pending")' >/dev/null <<<"$json"; then exit 8; fi
		exit 0
	fi
	;;
"run view")
	cat "$STATE/log-$job.txt"
	exit 0
	;;
"api graphql") json="$(cat "$STATE/graphql.json")" ;;
*) echo "fake gh: unexpected: $*" >&2; exit 3 ;;
esac
if [ -n "$jqf" ]; then jq -r "$jqf" <<<"$json"; else echo "$json"; fi
FAKE
chmod +x "$TMP/bin/gh"
export PATH="$TMP/bin:$PATH"
export FR_PR_WAIT_POLL=0

fail=0
pass() { echo "ok   $1"; }
bad() {
	echo "FAIL $1"
	fail=1
}
check() { # check <name> <command...>
	local name="$1"
	shift
	if "$@"; then pass "$name"; else bad "$name"; fi
}

URL=https://github.com/o/r/actions/runs
# c <name> <bucket> [job]: one check, as gh pr checks --json prints it.
c() {
	local done=2026-10-23T10:00:42Z
	[ "$2" != pending ] || done=0001-01-01T00:00:00Z
	jq -cn --arg n "$1" --arg b "$2" --arg d "$done" --arg u "$URL/100/job/${3:-1}" \
		'{name: $n, workflow: "ci", bucket: $b, state: ($b | ascii_upcase),
		  startedAt: "2026-10-23T10:00:00Z", completedAt: $d, link: $u}'
}
# snap <k> <check...>: what the k-th gh pr checks call returns.
snap() {
	local k="$1"
	shift
	printf '%s\n' "$@" | jq -s . >"$STATE/checks-$k.json"
}
# view <files...>: the PR, by alice, changing these files.
view() {
	printf '%s\n' "$@" | jq -Rn '{number: 5, url: "https://github.com/o/r/pull/5",
		author: {login: "alice"}, files: [inputs | select(. != "") | {path: .}]}' >"$STATE/view.json"
}
# reviews <json-reviews> <json-threads>
reviews() {
	jq -n --argjson r "$1" --argjson t "$2" \
		'{data: {repository: {pullRequest: {reviewDecision: null, reviews: {nodes: $r}, reviewThreads: {nodes: $t}}}}}' \
		>"$STATE/graphql.json"
}
setup() {
	export STATE="$TMP/state-$1"
	mkdir -p "$STATE"
	view README.md
	reviews '[]' '[]'
}
# run_it <args...>: runs the script; sets $rc, $out (stdout), $errout (stderr).
run_it() {
	rc=0
	"$SCRIPT" "$@" >"$STATE/out" 2>"$STATE/err" || rc=$?
	out="$(cat "$STATE/out")"
	errout="$(cat "$STATE/err")"
}
of_type() { jq -c --arg t "$1" 'select(.type == $t)' <<<"$out"; }
summary() { of_type summary | jq -r "$1"; }

# --- all pass, after polling -------------------------------------------------

setup pass
snap 1 "$(c test pending)" "$(c build pass)"
snap 2 "$(c test pass)" "$(c build pass)"
run_it 5
check "pass: exits 0" test "$rc" = 0
check "pass: polls until no check is pending" test "$(cat "$STATE/polls")" = 2
check "pass: one check line per check, with duration" \
	test "$(of_type check | jq -r '"\(.name) \(.conclusion) \(.duration_s)"')" = "test pass 42
build pass 42"
check "pass: verdict success" test "$(summary .verdict)" = success
# shellcheck disable=SC2329 # called through check
is_json() { jq -e . >/dev/null <<<"$out"; }
check "pass: stdout is only JSON" is_json
check "pass: progress goes to stderr" grep -q 'check test: pending' <<<"$errout"
check "pass: no failed log fetched" test -z "$(grep 'run view' "$STATE/calls" || true)"

# --- a failure stops the wait at once ----------------------------------------

setup failfast
snap 1 "$(c test fail 77)" "$(c build pending)"
snap 2 "$(c test fail 77)" "$(c build pass)"
printf '%s\n' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:01.1Z ##[group]Run actions/checkout@v7' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:02.1Z Syncing repository: o/r' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:03.1Z ##[group]Run go test ./...' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:04.1Z ok  \tpkg/a\t0.1s' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:05.1Z --- FAIL: TestLaunch (0.00s)' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:05.2Z     launch_test.go:12: want 1 instance, got 2' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:05.3Z FAIL\tpkg/b\t0.2s' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:06.1Z \x1b[31mmore\x1b[0m' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:07.1Z ##[error]Process completed with exit code 1.' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:08.1Z Post job cleanup.' \
	$'test\tUNKNOWN STEP\t2026-10-23T10:00:09.1Z error: cleanup noise' >"$STATE/log-77.txt"
run_it 5
check "failfast: exits 1" test "$rc" = 1
check "failfast: stops at the first failure, with checks still pending" test "$(cat "$STATE/polls")" = 1
check "failfast: verdict check_failed" test "$(summary .verdict)" = check_failed
check "failfast: reports the pending check as pending" test "$(summary .checks.pending)" = 1
check "failfast: pending check has no duration" \
	test "$(of_type check | jq -r 'select(.name == "build") | .duration_s')" = null
check "failfast: fetches the failed job's log" grep -q '^run view 100 --job 77 --log-failed' "$STATE/calls"
lines="$(of_type failed_log | jq -r '.lines[]')"
check "failfast: log keeps the failing test and its message" \
	grep -qxF '    launch_test.go:12: want 1 instance, got 2' <<<"$lines"
check "failfast: log keeps the error line, without prefix or timestamp" \
	grep -qxF '##[error]Process completed with exit code 1.' <<<"$lines"
check "failfast: log drops colours" grep -qxF 'more' <<<"$lines"
check "failfast: log drops earlier steps and passing lines" \
	test -z "$(grep -e Syncing -e 'pkg/a' <<<"$lines" || true)"
check "failfast: log drops what follows the error" \
	test -z "$(grep -e cleanup <<<"$lines" || true)"
check "failfast: failed_log names run and job" \
	test "$(of_type failed_log | jq -r '"\(.check) \(.run) \(.job)"')" = "test 100 77"
check "failfast: still reports reviews" test -n "$(of_type review)"

# --- a failure of a check that is not required does not stop the wait -------

setup optional
echo '[{"name":"build"}]' >"$STATE/required.json"
snap 1 "$(c lint fail 9)" "$(c build pending)"
snap 2 "$(c lint fail 9)" "$(c build pass)"
echo "--- FAIL: TestLint" >"$STATE/log-9.txt"
run_it 5
check "optional: waits for the required check" test "$(cat "$STATE/polls")" = 2
check "optional: still fails at the end" test "$rc:$(summary .verdict)" = 1:check_failed
check "optional: log without step markers is kept" \
	test "$(of_type failed_log | jq -r '.lines[]')" = "--- FAIL: TestLint"

# --- a required failure stops it ---------------------------------------------

setup required
echo '[{"name":"build"}]' >"$STATE/required.json"
snap 1 "$(c lint pending)" "$(c build fail 3)"
: >"$STATE/log-3.txt"
# lint stays pending: without the fail-fast, this would run into the deadline.
run_it 5 --deadline 3
check "required: stops at the required failure" test "$(cat "$STATE/polls")" = 1
check "required: exits 1" test "$rc" = 1

# --- no checks yet -----------------------------------------------------------

setup nochecks
snap 2 "$(c test pass)"
run_it 5
check "nochecks: waits while none is reported" test "$(cat "$STATE/polls")" = 2
check "nochecks: then succeeds" test "$rc" = 0

# --- threads -----------------------------------------------------------------

setup threads
snap 1 "$(c test pass)"
reviews '[{"author":{"login":"copilot-pull-request-reviewer"},"state":"COMMENTED"}]' '[
	{"isResolved":false,"isOutdated":false,"path":"a.go","line":7,
	 "comments":{"nodes":[{"author":{"login":"copilot-pull-request-reviewer"},"body":"leaks a VM","url":"u1"}]}},
	{"isResolved":false,"isOutdated":true,"path":"c.go","line":null,
	 "comments":{"nodes":[{"author":{"login":"bob"},"body":"still open","url":"u3"}]}},
	{"isResolved":true,"isOutdated":false,"path":"b.go","line":1,
	 "comments":{"nodes":[{"author":{"login":"bob"},"body":"fixed","url":"u2"}]}}]'
run_it 5
check "threads: an unresolved thread fails" test "$rc:$(summary .verdict)" = 1:unresolved_threads
check "threads: prints each unresolved thread, outdated or not" \
	test "$(of_type thread | jq -r '"\(.path):\(.line) \(.author) \(.body)"')" = "a.go:7 copilot-pull-request-reviewer leaks a VM
c.go:null bob still open"
check "threads: counts them" test "$(summary .unresolved_threads)" = 2
check "threads: prints the review" \
	test "$(of_type review | jq -r '.reviews[0] | "\(.author) \(.state)"')" = "copilot-pull-request-reviewer COMMENTED"

# --- the review gate ---------------------------------------------------------

setup gate
snap 1 "$(c test pass)"
view internal/scheduler/scheduler.go internal/provisioner/egress.go README.md
reviews '[{"author":{"login":"alice"},"state":"COMMENTED"}]' '[]'
run_it 5
check "gate: still exits 0" test "$rc" = 0
check "gate: names the gated paths" \
	test "$(of_type review_required | jq -r '.paths | join(" ")')" = "internal/scheduler/scheduler.go internal/provisioner/egress.go"
check "gate: the author's own review does not count" test "$(summary .review_required)" = true

setup gate-reviewed
snap 1 "$(c test pass)"
view internal/ec2/provisioner.go
reviews '[{"author":{"login":"copilot-pull-request-reviewer"},"state":"COMMENTED"}]' '[]'
run_it 5
check "gate: an independent review satisfies it" test -z "$(of_type review_required)"

setup gate-untouched
snap 1 "$(c test pass)"
view internal/config/config.go internal/ec2x/a.go
run_it 5
check "gate: other paths need no review" test "$(summary .review_required)" = false

# --- deadline, usage, --repo -------------------------------------------------

setup deadline
snap 1 "$(c test pending)"
run_it 5 --deadline 0
check "deadline: exits 124" test "$rc:$(summary .verdict)" = 124:deadline

setup usage
run_it
check "usage: no PR exits 2" test "$rc" = 2
run_it 5 --deadline soon
check "usage: a bad deadline exits 2" test "$rc" = 2
run_it 5 --bogus
check "usage: an unknown option exits 2" test "$rc" = 2
rm "$STATE/view.json"
run_it 5
check "usage: an unknown PR exits 2" test "$rc" = 2

setup repo
snap 1 "$(c test pass)"
run_it 5 --repo o/r
check "repo: --repo reaches every gh pr call" \
	test "$(grep -c '^pr ' "$STATE/calls")" = "$(grep -c '^pr .*--repo o/r' "$STATE/calls")"

exit "$fail"
