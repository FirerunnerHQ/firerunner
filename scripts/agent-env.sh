#!/usr/bin/env bash
# agent-env.sh — make the tools this repository needs reachable from a coding
# agent's shell. Run by the SessionStart hook in .claude/settings.json.
#
# An agent's Bash calls run in a non-interactive shell that does not read the
# login profile, so on macOS Homebrew's directory, and with it go, gh and the
# shell linter, is often missing from PATH. For each such tool that is not on
# PATH, this finds the first directory below that holds it and appends that
# directory to PATH for the rest of the session, by writing an export line to
# $CLAUDE_ENV_FILE, which Claude Code runs before every Bash command. A tool
# already on PATH is never shadowed; a directory that does not exist is
# skipped, so on a machine with everything on PATH this does nothing.
#
# It never changes git config. The sign-off hook is opt-in, because a DCO
# sign-off is a deliberate certification: until `make setup` has installed it,
# this prints a one-line reminder.
#
# Outside Claude Code ($CLAUDE_ENV_FILE unset) it prints the export lines, so
#   eval "$(scripts/agent-env.sh)"
# does the same for an ordinary shell. Every other line it prints is a comment.
#
# Directories searched, in order (override with FR_AGENT_ENV_DIRS, a
# colon-separated list): /opt/homebrew/bin /usr/local/go/bin /usr/local/bin
# ~/go/bin ~/.local/share/mise/shims ~/.local/bin
#
# It always exits 0: a session must start even if this cannot help.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 0

tools=(go gh shellcheck)

if [ -n "${FR_AGENT_ENV_DIRS:-}" ]; then
	IFS=: read -r -a dirs <<<"$FR_AGENT_ENV_DIRS"
else
	dirs=(/opt/homebrew/bin /usr/local/go/bin /usr/local/bin
		"$HOME/go/bin" "$HOME/.local/share/mise/shims" "$HOME/.local/bin")
fi

added=()
missing=()
newpath="$PATH"
for tool in "${tools[@]}"; do
	PATH="$newpath" command -v "$tool" >/dev/null 2>&1 && continue
	found=""
	for d in "${dirs[@]}"; do
		if [ -x "$d/$tool" ] && [ ! -d "$d/$tool" ]; then
			found="$d"
			break
		fi
	done
	if [ -n "$found" ]; then
		newpath="$newpath:$found"
		added+=("$found")
	else
		missing+=("$tool")
	fi
done

lines=()
for d in ${added[@]+"${added[@]}"}; do
	lines+=("export PATH=\"\$PATH:$d\"")
done

if [ "${#lines[@]}" -gt 0 ]; then
	if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
		printf '%s\n' "${lines[@]}" >>"$CLAUDE_ENV_FILE"
	else
		printf '%s\n' "${lines[@]}"
	fi
	echo "# agent-env: added to PATH: ${added[*]}"
fi
if [ "${#missing[@]}" -gt 0 ]; then
	echo "# agent-env: not found on PATH or in ${dirs[*]}: ${missing[*]}"
fi

if [ "$(git config --get core.hooksPath 2>/dev/null)" != .githooks ]; then
	echo "# agent-env: run \`make setup\` to sign off commits automatically"
fi
exit 0
