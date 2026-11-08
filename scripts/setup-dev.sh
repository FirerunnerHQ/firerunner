#!/usr/bin/env bash
# setup-dev.sh — one-time setup of a clone for development (`make setup`).
#
# Installs a copy of .githooks/commit-msg, which signs off every commit for
# the dco check, into the clone's own hooks directory (.git/hooks). A copy,
# not core.hooksPath pointing at .githooks: that would run whatever
# .githooks/commit-msg the checked-out branch holds, so checking out someone
# else's branch and committing would run their code as you. Rerun it to pick
# up a newer hook.
#
# Opt-in: running it means you certify the DCO for every commit you make in
# this clone, so nothing runs it for you. Idempotent. It never overwrites a
# commit-msg hook of your own, nor changes a core.hooksPath set to anything
# but .githooks; it says what to do instead. A core.hooksPath of .githooks,
# which earlier versions set, is removed in favour of the copy.
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

src=.githooks/commit-msg
# The marker line every copy carries, so a hook of the user's own is told
# apart from ours.
marker="# firerunner: commit-msg hook installed by make setup"

have="$(git config --get core.hooksPath || true)"
case "$have" in
"") ;;
.githooks)
	git config --unset core.hooksPath
	say "setup: removed core.hooksPath=.githooks, which ran each branch's own hook"
	;;
*)
	echo "setup: core.hooksPath is '$have'; not changing it." >&2
	echo "setup: to sign off commits automatically, copy $src into it as commit-msg." >&2
	exit 0
	;;
esac

hooks="$(git rev-parse --git-path hooks)"
dst="$hooks/commit-msg"
if [ -e "$dst" ] && ! grep -qxF "$marker" "$dst"; then
	echo "setup: $dst is a hook of your own; not replacing it." >&2
	echo "setup: to sign off commits automatically, call a copy of $src from it." >&2
	exit 0
fi
if [ -e "$dst" ] && cmp -s "$src" "$dst"; then
	say "setup: git hooks already installed ($dst)"
	exit 0
fi
mkdir -p "$hooks"
cp "$src" "$dst.tmp"
chmod +x "$dst.tmp"
mv "$dst.tmp" "$dst"
echo "setup: installed $dst: commits are signed off automatically"
