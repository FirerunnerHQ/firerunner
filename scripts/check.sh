#!/usr/bin/env bash
# check.sh — everything CI's test, build and shell jobs check that this machine
# can run, in one command, ending in a summary a person or an agent can act on:
#
#   PASS  gofmt
#   FAIL  test+coverage   (see the output above)
#   SKIP  docker-shim     needs Linux (/proc)
#
# Exit status is non-zero if any step failed. Skipped steps never fail the run
# but always say why, so "green here" never hides an unchecked step.
#
# Usage: scripts/check.sh [--fast]
#   --fast  skip the race/coverage run (the slowest step) for a quick loop
#
# TEST_TIMEOUT (default 4m) is go test -timeout per test binary, here and in
# make test/cover: a hung test fails instead of stalling the run.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

fast=0
for arg in "$@"; do
	case "$arg" in
	--fast) fast=1 ;;
	-h | --help)
		sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $arg" >&2
		exit 2
		;;
	esac
done

results=()
failed=0

# step <name> <command...>: run a step, record PASS or FAIL.
step() {
	local name="$1"
	shift
	echo "==> $name"
	if "$@"; then
		results+=("PASS  $name")
	else
		results+=("FAIL  $name")
		failed=1
	fi
}

# skip <name> <reason>: record a step this machine cannot run.
skip() {
	echo "==> $1 (skipped: $2)"
	results+=("SKIP  $1  $2")
}

# Step functions are passed to step by name, which shellcheck cannot see.
# shellcheck disable=SC2329
gofmt_clean() {
	local out
	# gofmt prints nothing to stdout for a file it cannot parse, so its exit
	# status is checked too: an empty list alone would read as clean.
	out="$(git ls-files -z '*.go' | xargs -0 gofmt -l)" || {
		echo "gofmt failed (a file does not parse?)"
		return 1
	}
	[ -z "$out" ] || {
		echo "not gofmt-clean:"
		echo "$out"
		return 1
	}
}

# shellcheck disable=SC2329
# signed_off: every commit on this branch since it left origin/main carries a
# Signed-off-by for its author, as the dco workflow requires.
signed_off() {
	local base missing=0
	base="$(git merge-base HEAD origin/main 2>/dev/null)" || {
		echo "no origin/main to compare against; run git fetch origin"
		return 1
	}
	for sha in $(git rev-list --no-merges "$base..HEAD"); do
		email="$(git log -1 --format='%ae' "$sha")"
		if ! git log -1 --format='%(trailers:key=Signed-off-by,valueonly)' "$sha" | grep -qiF "<$email>"; then
			echo "missing Signed-off-by: $(git log -1 --format='%h %s' "$sha")"
			missing=1
		fi
	done
	[ "$missing" = 0 ] || echo "fix: git rebase --signoff $base"
	return "$missing"
}

step gofmt gofmt_clean
step build go build ./...
step vet go vet ./...
step vet-linux env GOOS=linux go vet ./...
if [ "$fast" = 1 ]; then
	skip test+coverage "--fast"
	step test go test -timeout "${TEST_TIMEOUT:-4m}" ./...
else
	step test+coverage make --no-print-directory cover-check
fi
if [ "$(uname -s)" = Linux ]; then
	step docker-shim images/test-docker-shim.sh
else
	skip docker-shim "needs Linux (/proc)"
fi
step ec2-boot-script images/test-ec2-run.sh
step ec2-ami-build images/test-build-ec2-ami.sh
step mutate-script scripts/test-mutate.sh
if command -v jq >/dev/null; then
	step merge-stack scripts/test-merge-stack.sh
else
	skip merge-stack "jq not installed"
fi
if command -v shellcheck >/dev/null; then
	step shellcheck shellcheck images/assets/firerunner-ec2-run.sh images/test-ec2-run.sh \
		images/build-ec2-ami.sh images/ec2/provision.sh images/test-build-ec2-ami.sh scripts/check.sh \
		scripts/mutate.sh scripts/test-mutate.sh \
		scripts/merge-stack.sh scripts/test-merge-stack.sh
else
	skip shellcheck "shellcheck not installed"
fi
step dco-signoff signed_off

echo
echo "== summary"
printf '%s\n' "${results[@]}"
exit "$failed"
