#!/usr/bin/env bash
# setup-dev.sh — one-time setup of a clone for development (`make setup`).
#
# Points git at the repository's hooks in .githooks/, whose commit-msg hook
# signs off every commit for the dco check. Opt-in: running it means you
# certify the DCO for every commit you make in this clone, so nothing runs it
# for you. Idempotent: run it as often as you
# like. It never overrides a different core.hooksPath, nor hooks of your own
# in .git/hooks, which core.hooksPath would silently disable; it says what to
# do instead.
#
# Usage: scripts/setup-dev.sh [--quiet]
#   --quiet  print nothing when there is nothing to do
set -euo pipefail

cd "$(dirname "$0")/.." || exit 2

quiet=0
case "${1:-}" in
--quiet) quiet=1 ;;
"") ;;
*)
	echo "usage: scripts/setup-dev.sh [--quiet]" >&2
	exit 2
	;;
esac

say() { [ "$quiet" = 1 ] || echo "$@"; }

want=.githooks
have="$(git config --get core.hooksPath || true)"

if [ "$have" = "$want" ]; then
	say "setup: git hooks already installed (core.hooksPath=$want)"
	exit 0
fi

if [ -n "$have" ]; then
	echo "setup: core.hooksPath is already '$have'; not changing it." >&2
	echo "setup: to sign off commits automatically, call $want/commit-msg from your commit-msg hook." >&2
	exit 0
fi

hooks="$(git rev-parse --git-path hooks)"
own=()
if [ -d "$hooks" ]; then
	for f in "$hooks"/*; do
		[ -f "$f" ] && [ -x "$f" ] && [ "${f%.sample}" = "$f" ] && own+=("${f##*/}")
	done
fi
if [ "${#own[@]}" -gt 0 ]; then
	echo "setup: $hooks has hooks of its own (${own[*]}); core.hooksPath would disable them." >&2
	echo "setup: not installing. Move them into $want/, or run: git config core.hooksPath $want" >&2
	exit 0
fi

git config core.hooksPath "$want"
echo "setup: installed git hooks (core.hooksPath=$want): commits are signed off automatically"
