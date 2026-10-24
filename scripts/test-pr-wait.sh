#!/usr/bin/env bash
# Tests for scripts/pr-wait.sh against a fake gh. Needs bash and jq; no
# network. The fake serves the PR from $STATE/view.json, its head commit from
# line k of $STATE/heads on the k-th `gh pr view` (the last repeats; without
# the file, view.json's), one check-run snapshot per check-runs call on commit
# SHA ($STATE/runs-SHA-1.json, -2, …; the last repeats; none: no runs), the
# base branch's rules from $STATE/rules.json ([] without it; an error with
# $STATE/rules.err), workflow runs from $STATE/workflows.json, failed logs
# from $STATE/log-JOB.txt and GraphQL from $STATE/graphql.json. It counts
# check-runs calls in $STATE/polls and records every call in $STATE/calls.
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
jqf="" job="" args=()
while [ $# -gt 0 ]; do
	case "$1" in
	--jq) jqf="$2"; shift ;;
	--job) job="$2"; shift ;;
	--json|--repo|-F|-f) shift ;;
	--paginate|--log-failed) ;;
	*) args+=("$1") ;;
	esac
	shift
done
set -- "${args[@]}"
# next <counter>: bump $STATE/<counter> and print it.
next() {
	local n=$(($(cat "$STATE/$1" 2>/dev/null || echo 0) + 1))
	echo "$n" >"$STATE/$1"
	echo "$n"
}
case "$1 ${2:-}" in
"pr view")
	[ -f "$STATE/view.json" ] || { echo "no pull requests found for branch \"$3\"" >&2; exit 1; }
	json="$(cat "$STATE/view.json")"
	k="$(next views)"
	if [ -f "$STATE/heads" ]; then
		h="$(sed -n "${k}p" "$STATE/heads")"
		[ -n "$h" ] || h="$(tail -n 1 "$STATE/heads")"
		json="$(jq --arg h "$h" '.headRefOid = $h' <<<"$json")"
	fi
	;;
"api repos/o/r/commits/"*)
	sha="$(cut -d/ -f5 <<<"$2")"
	next polls >/dev/null
	[ ! -f "$STATE/runs-$sha.err" ] || { echo "HTTP 502: Bad Gateway" >&2; exit 1; }
	n="$(next "polls-$sha")"
	while [ "$n" -gt 1 ] && [ ! -f "$STATE/runs-$sha-$n.json" ]; do n=$((n - 1)); done
	if [ -f "$STATE/runs-$sha-$n.json" ]; then json="$(cat "$STATE/runs-$sha-$n.json")"; else json='{"total_count": 0, "check_runs": []}'; fi
	;;
"api repos/o/r/rules/branches/"*)
	[ ! -f "$STATE/rules.err" ] || { echo "HTTP 403: Resource not accessible" >&2; exit 1; }
	json="$(cat "$STATE/rules.json" 2>/dev/null || echo '[]')"
	;;
"api repos/o/r/actions/runs"*)
	json="$(cat "$STATE/workflows.json" 2>/dev/null || echo '{"workflow_runs": []}')"
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
# A clock that ticks once per call when $STATE/clock holds its start, so a
# deadline falls on a known poll; the real date otherwise.
cat >"$TMP/bin/date" <<'FAKE'
#!/bin/sh
if [ -f "${STATE:-}/clock" ]; then
	t="$(cat "$STATE/clock")"
	echo $((t + 1)) >"$STATE/clock"
	echo "$t"
else
	exec /bin/date "$@"
fi
FAKE
chmod +x "$TMP/bin/date"
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
H1=a1a1a1 # the PR's head, unless a test moves it
H2=b2b2b2
# c <name> <bucket> [job]: one check run, as the REST API returns it.
c() {
	local done=2026-10-23T10:00:42Z status=completed conclusion
	case "$2" in
	pass) conclusion=success ;;
	fail) conclusion=failure ;;
	cancel) conclusion=cancelled ;;
	skip) conclusion=skipped ;;
	pending) status=in_progress conclusion="" done="" ;;
	esac
	jq -cn --arg n "$1" --arg s "$status" --arg k "$conclusion" --arg d "$done" --arg u "$URL/100/job/${3:-1}" \
		'{name: $n, status: $s, conclusion: (if $k == "" then null else $k end),
		  started_at: "2026-10-23T10:00:00Z", completed_at: (if $d == "" then null else $d end),
		  details_url: $u, html_url: $u}'
}
# snap_at <sha> <k> <check...>: what the k-th check-runs call on sha returns.
snap_at() {
	local sha="$1" k="$2"
	shift 2
	printf '%s\n' "$@" | jq -s '{total_count: length, check_runs: .}' >"$STATE/runs-$sha-$k.json"
}
# snap <k> <check...>: the same, on the default head.
snap() { snap_at "$H1" "$@"; }
# rules <context...>: the base branch requires these checks.
rules() {
	printf '%s\n' "$@" | jq -Rn '[{type: "pull_request", parameters: {}},
		{type: "required_status_checks", parameters: {required_status_checks: [inputs | {context: ., integration_id: 1}]}}]' \
		>"$STATE/rules.json"
}
# view <files...>: the PR, by alice, at head $H1 on main, changing these files.
view() {
	printf '%s\n' "$@" | jq -Rn --arg h "$H1" '{number: 5, url: "https://github.com/o/r/pull/5",
		author: {login: "alice"}, headRefOid: $h, baseRefName: "main",
		files: [inputs | select(. != "") | {path: .}]}' >"$STATE/view.json"
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
echo '{"workflow_runs": [{"id": 100, "name": "ci"}, {"id": 7, "name": "dco"}]}' >"$STATE/workflows.json"
run_it 5
check "pass: exits 0" test "$rc" = 0
check "pass: names each check's workflow" test "$(of_type check | jq -r .workflow | sort -u)" = ci
check "pass: summary names the head" test "$(summary .head)" = "$H1"
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
rules build
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
rules build
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

# --- only the current head counts --------------------------------------------

# Just after a push: the old head is green, the new one has no run yet.
setup stale
view README.md
echo "$H2" >"$STATE/heads"
snap_at "$H1" 1 "$(c test pass)" "$(c build pass)"
snap_at "$H2" 2 "$(c test pending)" "$(c build pending)"
snap_at "$H2" 3 "$(c test pass)" "$(c build pass)"
run_it 5
check "stale: a head without check runs is pending, not green" test "$(cat "$STATE/polls")" = 3
check "stale: then succeeds on the new head" test "$rc:$(summary .head)" = "0:$H2"
check "stale: never reads the old head's checks" test -z "$(grep "commits/$H1" "$STATE/calls" || true)"

# A push while waiting: the old head's checks stop counting.
setup moved
printf '%s\n' "$H1" "$H1" "$H2" >"$STATE/heads"
snap_at "$H1" 1 "$(c test pending)"
snap_at "$H2" 1 "$(c test pending)"
snap_at "$H2" 2 "$(c test pass)"
run_it 5
check "moved: follows the head to the new commit" test "$rc:$(summary .head)" = "0:$H2"
check "moved: says so" grep -q "head moved to $H2" <<<"$errout"
check "moved: reports the new head's checks only" test "$(of_type check | jq -r .conclusion)" = pass

# The new head's checks cannot be read: the old head's are not reported.
# The clock reads 0 for the deadline, then 1 and 2 after polls 1 and 2: the
# deadline of 2 falls on poll 2, the first on the new head.
setup moved-error
rules sign-off
printf '%s\n' "$H1" "$H2" >"$STATE/heads"
snap_at "$H1" 1 "$(c test pass)"
touch "$STATE/runs-$H2.err"
echo 0 >"$STATE/clock"
run_it 5 --deadline 2
check "moved-error: ends in the deadline" test "$rc:$(summary .head)" = "124:$H2"
check "moved-error: drops the old head's checks" test "$(summary .checks.total)" = 0
check "moved-error: logs the error" grep -q 'Bad Gateway' <<<"$errout"

# GitHub still shows the old head; --sha names the one just pushed.
setup sha
printf '%s\n' "$H1" "$H1" "$H2" >"$STATE/heads"
snap_at "$H1" 1 "$(c test pass)"
snap_at "$H2" 1 "$(c test pass)"
run_it 5 --sha "$H2"
check "sha: does not accept the old head's green checks" test "$rc:$(summary .head)" = "0:$H2"
check "sha: never reads the old head's checks" test -z "$(grep "commits/$H1" "$STATE/calls" || true)"
setup sha-never
snap 1 "$(c test pass)"
run_it 5 --sha "$H2" --deadline 0
check "sha: a head that never arrives ends in the deadline" test "$rc:$(summary .verdict)" = 124:deadline
check "sha: and names it" grep -q "never reached head $H2" <<<"$errout"
run_it 5 --sha HEAD
check "sha: a ref name is a usage error" test "$rc" = 2

# --- required checks that have not started yet -------------------------------

# CI is done before the separate dco workflow registers sign-off.
setup late-required
rules test sign-off
snap 1 "$(c test pass)" "$(c build pass)"
snap 2 "$(c test pass)" "$(c build pass)" "$(c sign-off pending 7)"
snap 3 "$(c test pass)" "$(c build pass)" "$(c sign-off pass 7)"
run_it 5
check "late-required: waits for a required check not yet reported" test "$(cat "$STATE/polls")" = 3
check "late-required: then succeeds" test "$rc:$(summary .verdict)" = 0:success
check "late-required: reads the rules of the base branch" grep -q '^api repos/o/r/rules/branches/main ' "$STATE/calls"
check "late-required: never asks gh pr checks" test -z "$(grep '^pr checks' "$STATE/calls" || true)"
check "late-required: none missing in the end" test "$(summary '.missing_required | length')" = 0

setup never-required
rules test sign-off
snap 1 "$(c test pass)"
run_it 5 --deadline 0
check "never-required: ends in the deadline" test "$rc:$(summary .verdict)" = 124:deadline
check "never-required: names the missing check" grep -q 'never reported on .*: sign-off' <<<"$errout"
check "never-required: and lists it in the summary" test "$(summary '.missing_required | join(",")')" = sign-off

setup rules-error
touch "$STATE/rules.err"
snap 1 "$(c test pass)"
run_it 5
check "rules-error: falls back to every reported check" test "$rc" = 0
check "rules-error: says the required checks are unknown" grep -q 'required checks of main unknown' <<<"$errout"

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
