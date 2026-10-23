#!/usr/bin/env bash
# Tests for scripts/merge-stack.sh against a fake gh. Needs bash and jq; no
# network. The fake serves PRs, PR lists and issues from JSON files in $STATE,
# applies --jq with jq (gh's --jq is jq syntax), and records every call that
# would change something in $STATE/writes.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/merge-stack.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
jqf="" base="" state="" args=()
while [ $# -gt 0 ]; do
	case "$1" in
	--jq) jqf="$2"; shift ;;
	--base) base="$2"; args+=("$1" "$2"); shift ;;
	--state) state="$2"; shift ;;
	--json|--limit|--comment) args+=("$1" "$2"); shift ;;
	*) args+=("$1") ;;
	esac
	shift
done
set -- "${args[@]}"
[ ! -f "$STATE/fail-$1-$2" ] || { echo "gh: injected failure" >&2; exit 1; }
case "$1 $2" in
"repo view") json='{"defaultBranchRef":{"name":"main"}}' ;;
"pr view") json="$(cat "$STATE/pr-$3.json")" ;;
"pr list")
	f="$STATE/list-${base//\//_}-$state.json"
	if [ -f "$f" ]; then json="$(cat "$f")"; else json='[]'; fi
	;;
"issue view")
	s=OPEN
	[ ! -f "$STATE/issue-$3" ] || s="$(cat "$STATE/issue-$3")"
	json="{\"state\":\"$s\"}"
	;;
"pr edit"|"pr merge"|"issue close")
	printf '%s\n' "$*" >>"$STATE/writes"
	case "$1 $2" in
	"issue close") echo CLOSED >"$STATE/issue-$3" ;;
	"pr merge")
		jq '.state = "MERGED" | .mergeCommit = {oid: "abc123"}' "$STATE/pr-$3.json" >"$STATE/merged"
		mv "$STATE/merged" "$STATE/pr-$3.json"
		;;
	esac
	exit 0
	;;
*) echo "fake gh: unexpected: $*" >&2; exit 3 ;;
esac
if [ -n "$jqf" ]; then jq -r "$jqf" <<<"$json"; else echo "$json"; fi
FAKE
chmod +x "$TMP/bin/gh"
export PATH="$TMP/bin:$PATH"

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

# pr <n> <head> <base> <state> <body>
pr() {
	jq -n --argjson n "$1" --arg h "$2" --arg b "$3" --arg s "$4" --arg body "$5" \
		'{number: $n, headRefName: $h, baseRefName: $b, state: $s, body: $body,
		  url: "https://github.com/o/r/pull/\($n)",
		  mergeCommit: (if $s == "MERGED" then {oid: "def456"} else null end)}' \
		>"$STATE/pr-$1.json"
}
# list <base> <state> <pr...>: what gh pr list --base --state returns.
list() {
	local f="$STATE/list-${1//\//_}-$2.json"
	shift 2
	local n
	for n in "$@"; do cat "$STATE/pr-$n.json"; done | jq -s . >"$f"
}

# A stack on #10 (feat/a -> main): two open PRs on feat/a; #13 and #14
# merged into feat/a; #15 merged into #13's branch.
setup_stack() {
	export STATE="$TMP/state-$1"
	mkdir -p "$STATE"
	pr 10 feat/a main OPEN "Closes #1"
	pr 11 feat/b feat/a OPEN "Closes #7"
	pr 12 feat/c feat/a OPEN ""
	pr 13 feat/d feat/a MERGED $'Fixes #2\nresolves: #3, and closes other/repo#9.\nCloses #1'
	pr 14 feat/e feat/a MERGED "No issue."
	pr 15 feat/f feat/d MERGED $'CLOSES #4\nCloses #2'
	list feat/a open 11 12
	list feat/a merged 13 14
	list feat/d merged 15
	echo CLOSED >"$STATE/issue-3"
}

# --- dry run -----------------------------------------------------------------

setup_stack dry
out="$("$SCRIPT" 10)"
expect="#10: feat/a -> main (https://github.com/o/r/pull/10)
retarget  #11 (feat/b) from feat/a to main
retarget  #12 (feat/c) from feat/a to main
merge     #10 with --merge --delete-branch
close     #2 (from #13, merged into feat/a)
skip      #3 (from #13): already closed
close     #4 (from #15, merged into feat/d)

dry run: nothing changed. Rerun with --yes to do this."
check "dry run prints the whole plan" test "$out" = "$expect"
check "dry run changes nothing" test ! -e "$STATE/writes"
check "dry run plan leaves out the issue the merge itself closes" \
	test -z "$(grep '#1 ' <<<"$out" || true)"
check "dry run ignores another repository's issue" \
	test -z "$(grep '#9' <<<"$out" || true)"

# --- --yes -------------------------------------------------------------------

setup_stack yes
"$SCRIPT" --yes 10 >/dev/null
check "--yes retargets, then merges, then closes, in that order" \
	test "$(cut -d' ' -f1-4 "$STATE/writes")" = "pr edit 11 --base
pr edit 12 --base
pr merge 10 --merge
issue close 2 --comment
issue close 4 --comment"
check "--yes retargets the children to main" \
	test "$(grep -c '^pr edit 1[12] --base main$' "$STATE/writes")" = 2
check "--yes deletes the merged branch" grep -q '^pr merge 10 --merge --delete-branch$' "$STATE/writes"
check "--yes links the merge in the closing comment" \
	grep -qxF "issue close 2 --comment Fixed by #13, which was merged into \`feat/a\`; that landed in \`main\` with #10 (abc123)." "$STATE/writes"

# --- refusals ----------------------------------------------------------------

setup_stack retarget-fails
touch "$STATE/fail-pr-edit"
if "$SCRIPT" --yes 10 >/dev/null 2>&1; then bad "a failed retarget stops the script"; else pass "a failed retarget stops the script"; fi
check "a failed retarget leaves the base unmerged" test ! -e "$STATE/writes"

setup_stack list-fails
touch "$STATE/fail-pr-list"
if "$SCRIPT" --yes 10 >/dev/null 2>&1; then bad "a failed PR listing stops the script"; else pass "a failed PR listing stops the script"; fi
check "a failed PR listing changes nothing" test ! -e "$STATE/writes"

setup_stack not-bottom
if "$SCRIPT" --yes 11 >/dev/null 2>"$TMP/err"; then bad "refuses a PR not based on main"; else pass "refuses a PR not based on main"; fi
check "says to merge the bottom of the stack first" grep -q "merge the bottom of the stack first" "$TMP/err"
check "a refused PR changes nothing" test ! -e "$STATE/writes"

setup_stack merged
if "$SCRIPT" --yes 13 >/dev/null 2>"$TMP/err"; then bad "refuses a PR that is not open"; else pass "refuses a PR that is not open"; fi
check "says the PR is not open" grep -q "#13 is MERGED, not open" "$TMP/err"

status=0
"$SCRIPT" >/dev/null 2>&1 || status=$?
check "no PR is a usage error" test "$status" = 2
status=0
"$SCRIPT" --force 10 >/dev/null 2>&1 || status=$?
check "an unknown option is a usage error" test "$status" = 2

# --- a lone PR ---------------------------------------------------------------

export STATE="$TMP/state-lone"
mkdir -p "$STATE"
pr 20 feat/z main OPEN ""
"$SCRIPT" --yes '#20' >/dev/null
check "a PR without a stack is just merged" \
	test "$(cat "$STATE/writes")" = "pr merge 20 --merge --delete-branch"

if [ "$fail" = 0 ]; then echo "all merge-stack tests passed"; fi
exit "$fail"
