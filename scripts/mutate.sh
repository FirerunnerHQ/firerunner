#!/usr/bin/env bash
# mutate.sh — prove a test can fail: break one piece of code on purpose, run
# the tests, and restore the file byte-for-byte, whatever happens.
#
# Usage: scripts/mutate.sh FILE 'old text' 'new text' [-- go test args...]
#        scripts/mutate.sh FILE 'old text' 'new text' --exec CMD [ARGS...]
#
#   FILE        the file to break; 'old text' must occur in it exactly once
#   -- ARGS     replace the default go test arguments (FILE's package) with
#               ARGS, run from the current directory; name the packages,
#               e.g. -- -race -run TestLaunch ./internal/scheduler
#   --exec CMD  run CMD [ARGS...] instead of go test, from the current
#               directory, for tests that are not Go, e.g.
#               --exec images/test-ec2-run.sh
#               Everything after --exec is the command, as given (no shell
#               parsing); for pipes, use --exec bash -c '...'
#
# Steps, each decided by exit status, never by grepping test output:
#   1. baseline  the tests pass on the unchanged code (else the verdict means
#                nothing); MUTATE_BASELINE=0 skips it
#   2. mutate    replace the text; the diff is printed
#   3. check     the mutant still parses, so a syntax error is never counted
#                as caught: for a .go FILE its package and tests compile
#                (go test -run '^$'); for a shell script (.sh, .bash, or a
#                sh/bash #! line) bash -n or sh -n; any other file has no
#                check, and the output says so
#   4. test      run the tests against the mutant
#
# The last line of output is the verdict:
#   CAUGHT    the tests failed on the mutant (a timeout counts: it hung)
#   SURVIVED  the tests passed on the mutant: no test guards this code
#   INVALID   no verdict: bad usage, text missing or not unique, the
#             mutant does not compile or parse, or the baseline already fails
#
# Exit status: 0 CAUGHT, 1 SURVIVED, 2 INVALID, 3 the file could not be
# restored (it says where the original is; restore it by hand).
#
# The original is kept in a temporary directory and copied back on every
# exit path (success, failure, Ctrl-C, SIGTERM). Do not edit FILE meanwhile.
#
# Environment:
#   MUTATE_TIMEOUT   deadline per run (default 120s): go test -timeout, or for
#                    --exec seconds as N, Ns or Nm; at the deadline the
#                    command's process group gets SIGTERM, then SIGKILL
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
mode=go custom=0
if [ "$#" -gt 0 ]; then
	case "$1" in
	--)
		shift
		custom=1
		;;
	--exec)
		shift
		[ "$#" -gt 0 ] || verdict "INVALID usage: --exec needs a command"
		mode="exec"
		;;
	*) verdict "INVALID usage: unexpected argument '$1' (go test arguments go after --, a test command after --exec)" ;;
	esac
fi
timeout="${MUTATE_TIMEOUT:-120s}"
baseline="${MUTATE_BASELINE:-1}"

if [ "$mode" = exec ]; then
	case "$timeout" in
	*m) secs="${timeout%m}" unit=60 ;;
	*s) secs="${timeout%s}" unit=1 ;;
	*) secs="$timeout" unit=1 ;;
	esac
	case "$secs" in '' | *[!0-9]*) secs=0 ;; esac
	secs=$((10#$secs * unit))
	[ "$secs" -gt 0 ] ||
		verdict "INVALID MUTATE_TIMEOUT='$timeout': with --exec use whole seconds as N, Ns or Nm"
fi

[ -f "$file" ] || verdict "INVALID $file: no such file"
[ -n "$old" ] || verdict "INVALID old text is empty"
[ "$old" != "$new" ] || verdict "INVALID old and new text are the same"

# Read the file exactly, trailing newlines included (the x guards them from
# $(...)). Source files hold no NUL bytes, which a shell variable cannot.
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

# How the mutant is checked to still parse (step 3). go test compiles
# whatever it runs, so without --exec the check is always go's.
check=none
case "$mode:$file" in
go:* | exec:*.go) check=go ;;
exec:*.sh | exec:*.bash) check=bash ;;
exec:*)
	case "${content%%$'\n'*}" in
	'#!'*bash*) check=bash ;;
	'#!/bin/sh'* | '#!'*'env sh'*) check="sh" ;;
	esac
	;;
esac

tmp="$(mktemp -d "${TMPDIR:-/tmp}/mutate.XXXXXX")" || verdict "INVALID cannot create a temporary directory"
child=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
	stop_child
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# stop_child: end a running --exec command and everything it started.
stop_child() {
	[ -n "$child" ] || return 0
	kill -TERM -- "-$child" 2>/dev/null
	kill -KILL -- "-$child" 2>/dev/null
	child=""
}

# run_tests [flags...]: go test against the mutant's tests. By default that
# is FILE's own package, run in its directory so this works from anywhere in
# the module; after --, the caller's arguments, run from here (theirs come
# after ours, so their -timeout wins). -vet=off: a vet complaint about the
# mutant is not a failing test.
dir="$(dirname "$file")"
extra=("$@")
target="$dir"
[ "$custom" = 0 ] || target="$*"
run_tests() {
	if [ "$custom" = 1 ]; then
		go test -count=1 -vet=off -timeout "$timeout" ${extra[@]+"${extra[@]}"} "$@"
	else
		(cd "$dir" && go test -count=1 -vet=off -timeout "$timeout" "$@" .)
	fi
}

# run_exec: the --exec command with a deadline, without timeout(1). set -m
# gives the command and the watchdog process groups of their own, so the
# deadline (or an interrupted mutate.sh) ends whatever the command started,
# and the watchdog's own sleep is ended with it. Exit 124 on the deadline.
run_exec() {
	local dog rc
	rm -f "$tmp/timedout"
	set -m
	"${extra[@]}" </dev/null &
	child=$!
	(
		sleep "$secs"
		: >"$tmp/timedout"
		kill -TERM -- "-$child" 2>/dev/null
		sleep 5
		kill -KILL -- "-$child" 2>/dev/null
	) </dev/null >/dev/null 2>&1 &
	dog=$!
	set +m
	wait "$child"
	rc=$?
	kill -TERM -- "-$dog" 2>/dev/null
	wait "$dog" 2>/dev/null
	if [ -e "$tmp/timedout" ]; then
		echo "mutate.sh: the command passed its ${secs}s deadline (MUTATE_TIMEOUT); stopped it" >&2
		rc=124
	fi
	# Whatever the command left running in its group goes too.
	stop_child
	return "$rc"
}

run() {
	if [ "$mode" = exec ]; then run_exec; else run_tests; fi
}
if [ "$mode" = exec ]; then
	shown="${extra[*]}"
else
	shown="go test $target"
fi

if [ "$baseline" != 0 ]; then
	echo "==> baseline: $shown"
	run >"$tmp/baseline.log" 2>&1
	rc=$?
	if [ "$rc" != 0 ]; then
		cat "$tmp/baseline.log"
		verdict "INVALID tests fail before mutating (exit $rc); fix them or the arguments first"
	fi
fi

cp -p "$file" "$tmp/original" || verdict "INVALID cannot back up $file"
# shellcheck disable=SC2329 # invoked by the EXIT trap
restore() {
	stop_child
	if cp "$tmp/original" "$file" && cmp -s "$tmp/original" "$file"; then
		rm -rf "$tmp"
	else
		echo "mutate.sh: COULD NOT RESTORE $file; the original is at $tmp/original" >&2
		exit 3
	fi
}
trap restore EXIT

printf '%s' "$mutant" >"$file" || verdict "INVALID cannot write $file"
echo "==> mutant"
diff -u "$tmp/original" "$file" | sed '1,2d'

# check_mutant: step 3, with its output in $tmp/check.log.
check_mutant() {
	case "$check" in
	go)
		echo "==> compile: go test -run '^\$' $target"
		run_tests -run '^$' >"$tmp/check.log" 2>&1
		;;
	bash | sh)
		echo "==> parse: $check -n $file"
		"$check" -n "$file" >"$tmp/check.log" 2>&1
		;;
	*) echo "==> no syntax check for $file: make sure a CAUGHT is a failing test, not a broken file" ;;
	esac
}
if ! check_mutant; then
	cat "$tmp/check.log"
	verdict "INVALID mutant does not compile or parse (see above); this is not a caught mutant"
fi

echo "==> test: $shown"
run
rc=$?
if [ "$rc" = 0 ]; then
	verdict "SURVIVED tests pass with the mutant in $file: no test guards this code"
fi
verdict "CAUGHT tests fail with the mutant (exit $rc)"
