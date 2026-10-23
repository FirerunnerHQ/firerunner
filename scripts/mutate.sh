#!/usr/bin/env bash
# mutate.sh — prove a test can fail: break one piece of code on purpose, run
# the tests, and restore the file byte-for-byte, whatever happens.
#
# Usage: scripts/mutate.sh FILE 'old text' 'new text' [-- go test args...]
#
#   FILE        a Go source file; 'old text' must occur in it exactly once
#   -- ARGS     replace the default go test arguments (FILE's package) with
#               ARGS, run from the current directory; name the packages,
#               e.g. -- -race -run TestLaunch ./internal/scheduler
#
# Steps, each decided by exit status, never by grepping test output:
#   1. baseline  the tests pass on the unchanged code (else the verdict means
#                nothing); MUTATE_BASELINE=0 skips it
#   2. mutate    replace the text; the diff is printed
#   3. compile   the mutant and its tests compile (go test -run '^$')
#   4. test      run the tests against the mutant
#
# The last line of output is the verdict:
#   CAUGHT    the tests failed on the mutant (a timeout counts: it hung)
#   SURVIVED  the tests passed on the mutant: no test guards this code
#   INVALID   no verdict: bad usage, text missing or not unique, the
#             mutant does not compile, or the baseline already fails
#
# Exit status: 0 CAUGHT, 1 SURVIVED, 2 INVALID, 3 the file could not be
# restored (it says where the original is; restore it by hand).
#
# The original is kept in a temporary directory and copied back on every
# exit path (success, failure, Ctrl-C, SIGTERM). Do not edit FILE meanwhile.
#
# Environment:
#   MUTATE_TIMEOUT   go test -timeout per run (default 120s)
#   MUTATE_BASELINE  0 to skip the baseline run (default 1)
#
# Portable to macOS bash 3.2 and Linux: no timeout(1), no sed -i.
set -uo pipefail
export LC_ALL=C # byte semantics for the text search

verdict() {
	echo "$1"
	case "${1%% *}" in
	CAUGHT) exit 0 ;;
	SURVIVED) exit 1 ;;
	*) exit 2 ;;
	esac
}

usage() {
	awk 'NR == 1 { next } !/^#/ { exit } { sub(/^# ?/, ""); print }' "$0"
}

case "${1:-}" in -h | --help)
	usage
	exit 0
	;;
esac
if [ "$#" -lt 3 ]; then
	usage >&2
	verdict "INVALID usage: need FILE 'old text' 'new text'"
fi
file="$1" old="$2" new="$3"
shift 3
custom=0
if [ "$#" -gt 0 ]; then
	[ "$1" = "--" ] || verdict "INVALID usage: unexpected argument '$1' (go test arguments go after --)"
	shift
	custom=1
fi
timeout="${MUTATE_TIMEOUT:-120s}"
baseline="${MUTATE_BASELINE:-1}"

[ -f "$file" ] || verdict "INVALID $file: no such file"
[ -n "$old" ] || verdict "INVALID old text is empty"
[ "$old" != "$new" ] || verdict "INVALID old and new text are the same"

# Read the file exactly, trailing newlines included (the x guards them from
# $(...)). Go source holds no NUL bytes, which a shell variable cannot.
content="$(
	cat "$file"
	printf x
)" || verdict "INVALID $file: cannot read"
content="${content%x}"

case "$content" in
*"$old"*) ;;
*) verdict "INVALID old text not found in $file" ;;
esac
before="${content%%"$old"*}"
# Look for a second occurrence starting anywhere after the first one's
# first byte, so overlapping matches count too.
case "${content:$((${#before} + 1))}" in
*"$old"*) verdict "INVALID old text occurs more than once in $file; quote more context" ;;
esac
mutant="$before$new${content#*"$old"}"

# run_tests [flags...]: go test against the mutant's tests. By default that
# is FILE's own package, run in its directory so this works from anywhere in
# the module; after --, the caller's arguments, run from here (theirs come
# after ours, so their -timeout wins). -vet=off: a vet complaint about the
# mutant is not a failing test.
dir="$(dirname "$file")"
extra=("$@")
target="${*:-$dir}"
run_tests() {
	if [ "$custom" = 1 ]; then
		go test -count=1 -vet=off -timeout "$timeout" ${extra[@]+"${extra[@]}"} "$@"
	else
		(cd "$dir" && go test -count=1 -vet=off -timeout "$timeout" "$@" .)
	fi
}

if [ "$baseline" != 0 ]; then
	echo "==> baseline: go test $target"
	log="$(run_tests 2>&1)"
	rc=$?
	if [ "$rc" != 0 ]; then
		echo "$log"
		verdict "INVALID tests fail before mutating (exit $rc); fix them or the arguments first"
	fi
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/mutate.XXXXXX")" || verdict "INVALID cannot create a temporary directory"
cp -p "$file" "$tmp/original" || verdict "INVALID cannot back up $file"
# shellcheck disable=SC2329 # invoked by the EXIT trap
restore() {
	if cp "$tmp/original" "$file" && cmp -s "$tmp/original" "$file"; then
		rm -rf "$tmp"
	else
		echo "mutate.sh: COULD NOT RESTORE $file; the original is at $tmp/original" >&2
		exit 3
	fi
}
trap restore EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s' "$mutant" >"$file" || verdict "INVALID cannot write $file"
echo "==> mutant"
diff -u "$tmp/original" "$file" | sed '1,2d'

echo "==> compile: go test -run '^\$' $target"
if ! run_tests -run '^$' >"$tmp/compile.log" 2>&1; then
	cat "$tmp/compile.log"
	verdict "INVALID mutant does not compile (see above); this is not a caught mutant"
fi

echo "==> test: go test $target"
run_tests
rc=$?
if [ "$rc" = 0 ]; then
	verdict "SURVIVED tests pass with the mutant in $file: no test guards this code"
fi
verdict "CAUGHT tests fail with the mutant (go test exit $rc)"
