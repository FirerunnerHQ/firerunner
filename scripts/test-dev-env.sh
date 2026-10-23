#!/usr/bin/env bash
# Tests for the development-environment helpers: the commit-msg hook in
# .githooks/, scripts/setup-dev.sh and scripts/agent-env.sh. Needs bash and
# git; no network. Every git repository is a throwaway one under a temp dir,
# with the user's global and system git config shut out.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

export HOME="$TMP/home"
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL="$TMP/gitconfig"
mkdir -p "$HOME"
: >"$GIT_CONFIG_GLOBAL"
git config --global user.name "Ada Tester"
git config --global user.email "ada@example.com"
git config --global init.defaultBranch main
unset CLAUDE_ENV_FILE FR_AGENT_ENV_DIRS

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
# shellcheck disable=SC2329 # called through check
not() { ! "$@"; }

# newrepo <dir>: a fresh repository holding a copy of the files under test.
newrepo() {
	git init -q "$1"
	mkdir -p "$1/scripts" "$1/.githooks"
	cp "$ROOT/.githooks/commit-msg" "$1/.githooks/"
	cp "$ROOT/scripts/setup-dev.sh" "$ROOT/scripts/agent-env.sh" "$1/scripts/"
}

signoffs() { # signoffs <repo>: the Signed-off-by values of HEAD, one per line
	git -C "$1" log -1 --format='%(trailers:key=Signed-off-by,valueonly)' | sed '/^$/d'
}

# --- commit-msg hook -------------------------------------------------------

R="$TMP/hook"
newrepo "$R"
git -C "$R" config core.hooksPath .githooks
me="Ada Tester <ada@example.com>"

git -C "$R" commit -q --allow-empty -m "pkg: plain commit"
check "hook signs off a commit made without -s" \
	test "$(signoffs "$R")" = "$me"

git -C "$R" commit -q --allow-empty -s -m "pkg: commit with -s"
check "hook adds no duplicate when -s is used" \
	test "$(signoffs "$R")" = "$me"

git -C "$R" commit -q --allow-empty -m "pkg: subject" -m "Body." \
	-m "Signed-off-by: Bob Other <bob@example.com>"
check "hook adds ours beside someone else's sign-off" \
	test "$(signoffs "$R")" = "Bob Other <bob@example.com>
$me"

git -C "$R" commit -q --allow-empty --author "Cy Author <cy@example.com>" -m "pkg: other author"
check "hook signs off as the commit's author" \
	test "$(signoffs "$R")" = "Cy Author <cy@example.com>"

printf 'pkg: from an editor\n\nBody.\n# a comment\n' >"$TMP/msg"
GIT_EDITOR="cp $TMP/msg" git -C "$R" commit -q --allow-empty
check "hook signs off a message written in an editor" \
	test "$(signoffs "$R")" = "$me"
check "hook keeps the body and drops the comment" \
	test "$(git -C "$R" log -1 --format=%B | sed '/^$/d')" = "pkg: from an editor
Body.
Signed-off-by: $me"

before="$(git -C "$R" rev-parse HEAD)"
printf '# only a comment\n' >"$TMP/msg"
if GIT_EDITOR="cp $TMP/msg" git -C "$R" commit -q --allow-empty 2>/dev/null; then
	bad "hook lets git abort an empty message"
else
	check "hook lets git abort an empty message" \
		test "$(git -C "$R" rev-parse HEAD)" = "$before"
fi

# --- setup-dev.sh ------------------------------------------------------------

R="$TMP/setup"
newrepo "$R"
out="$("$R/scripts/setup-dev.sh")"
check "setup installs the hooks" \
	test "$(git -C "$R" config core.hooksPath)" = .githooks
check "setup says it installed them" grep -q "installed git hooks" <<<"$out"
check "setup is quiet the second time with --quiet" \
	test -z "$("$R/scripts/setup-dev.sh" --quiet)"
git -C "$R" commit -q --allow-empty -m "pkg: after setup"
check "a commit after setup is signed off" test "$(signoffs "$R")" = "$me"

R="$TMP/setup-other"
newrepo "$R"
git -C "$R" config core.hooksPath /somewhere/else
"$R/scripts/setup-dev.sh" 2>"$TMP/err"
check "setup leaves another core.hooksPath alone" \
	test "$(git -C "$R" config core.hooksPath)" = /somewhere/else
check "setup says why it did nothing" grep -q "not changing it" "$TMP/err"

R="$TMP/setup-own"
newrepo "$R"
printf '#!/bin/sh\nexit 0\n' >"$R/.git/hooks/pre-commit"
chmod +x "$R/.git/hooks/pre-commit"
"$R/scripts/setup-dev.sh" 2>"$TMP/err"
check "setup does not disable hooks of the user's own" \
	test -z "$(git -C "$R" config core.hooksPath || true)"
check "setup names the hook it would disable" grep -q "pre-commit" "$TMP/err"

# --- agent-env.sh ------------------------------------------------------------

# A PATH with only what the script itself needs, and no go/gh/shellcheck.
SYS="$TMP/sys"
mkdir -p "$SYS"
for t in bash env git sed dirname cat; do
	ln -s "$(command -v "$t")" "$SYS/$t"
done
# fake <dir> <tool...>: executables that print their own path.
fake() {
	local d="$1"
	shift
	mkdir -p "$d"
	for t in "$@"; do
		# shellcheck disable=SC2016 # $0 is for the fake, not for us
		printf '#!/bin/sh\necho "$0"\n' >"$d/$t"
		chmod +x "$d/$t"
	done
}
fake "$TMP/brew" go gh
fake "$TMP/later" go shellcheck
mkdir -p "$TMP/dir-named-go/go"
dirs="$TMP/absent:$TMP/dir-named-go:$TMP/brew:$TMP/later"

R="$TMP/agent"
newrepo "$R"
envfile="$TMP/claude-env"
: >"$envfile"
config_before="$(git -C "$R" config --list --show-origin)"
out="$(PATH="$SYS" CLAUDE_ENV_FILE="$envfile" FR_AGENT_ENV_DIRS="$dirs" "$R/scripts/agent-env.sh")"
check "agent-env writes one line per directory it adds" \
	test "$(cat "$envfile")" = "export PATH=\"\$PATH:$TMP/brew\"
export PATH=\"\$PATH:$TMP/later\""
resolved="$(PATH="$SYS" bash -c ". '$envfile'; go; gh; shellcheck")"
check "agent-env finds each tool in the first directory that has it" \
	test "$resolved" = "$TMP/brew/go
$TMP/brew/gh
$TMP/later/shellcheck"
check "agent-env reports what it added, as a comment" \
	grep -q "^# agent-env: added to PATH: $TMP/brew $TMP/later\$" <<<"$out"
check "agent-env does not install the git hooks" \
	test -z "$(git -C "$R" config core.hooksPath || true)"
check "agent-env leaves git config unchanged" \
	test "$(git -C "$R" config --list --show-origin)" = "$config_before"
check "agent-env suggests make setup while the hook is not installed" \
	grep -qxF "# agent-env: run \`make setup\` to sign off commits automatically" <<<"$out"
check "agent-env prints only comments under Claude Code" \
	test -z "$(grep -v '^#' <<<"$out" || true)"

: >"$envfile"
fake "$TMP/onpath" go gh shellcheck
git -C "$R" config core.hooksPath .githooks
out="$(PATH="$SYS:$TMP/onpath" CLAUDE_ENV_FILE="$envfile" FR_AGENT_ENV_DIRS="$dirs" "$R/scripts/agent-env.sh")"
check "agent-env never shadows a tool already on PATH" test ! -s "$envfile"
check "agent-env is silent with the tools on PATH and the hook installed" test -z "$out"

: >"$envfile"
out="$(PATH="$SYS" CLAUDE_ENV_FILE="$envfile" FR_AGENT_ENV_DIRS="$TMP/absent" "$R/scripts/agent-env.sh")"
status=$?
check "agent-env exits 0 when nothing is found" test "$status" = 0
check "agent-env writes nothing when nothing is found" test ! -s "$envfile"
check "agent-env names the tools it could not find" \
	grep -q "not found .*: go gh shellcheck\$" <<<"$out"

out="$(PATH="$SYS" FR_AGENT_ENV_DIRS="$dirs" "$R/scripts/agent-env.sh")"
check "agent-env prints exports for eval outside Claude Code" \
	test "$(PATH="$SYS" bash -c "eval '$out'; go")" = "$TMP/brew/go"

# .claude/e2e.env: exported for the session, through scripts/e2e/env.sh.
mkdir -p "$R/scripts/e2e" "$R/.claude"
cp "$ROOT/scripts/e2e/env.sh" "$R/scripts/e2e/"
printf '%s\n' "AWS_REGION=region-1" "AWS_PROFILE='it''s'" "FR_OWNER=e2e-mine" "NOT_OURS=1" >"$R/.claude/e2e.env"
: >"$envfile"
out="$(env -u AWS_REGION -u AWS_PROFILE -u FR_OWNER -u FR_E2E_ENV_FILE PATH="$SYS:$TMP/onpath" \
	CLAUDE_ENV_FILE="$envfile" FR_AGENT_ENV_DIRS="$dirs" "$R/scripts/agent-env.sh")"
check "agent-env exports .claude/e2e.env for the session" \
	test "$(env -u AWS_REGION -u AWS_PROFILE PATH="$SYS" bash -c ". '$envfile'; echo \"\$AWS_REGION|\$AWS_PROFILE\"")" = "region-1|it''s"
check "agent-env never exports the per-session owner" not grep -q FR_OWNER "$envfile"
check "agent-env exports only AWS and FR_ variables" not grep -q NOT_OURS "$envfile"
check "agent-env names what it exported" grep -qxF "# agent-env: from .claude/e2e.env: AWS_REGION AWS_PROFILE" <<<"$out"
: >"$envfile"
AWS_REGION=already PATH="$SYS:$TMP/onpath" CLAUDE_ENV_FILE="$envfile" FR_AGENT_ENV_DIRS="$dirs" "$R/scripts/agent-env.sh" >/dev/null
check "agent-env leaves a variable already set alone" not grep -q AWS_REGION "$envfile"
rm -r "$R/scripts/e2e" "$R/.claude"

# The real default directory list must not fail on this machine either.
: >"$envfile"
# shellcheck disable=SC2329 # called through check
quietly() { "$@" >/dev/null; }
check "agent-env with the default directories exits 0" \
	quietly env CLAUDE_ENV_FILE="$envfile" "$R/scripts/agent-env.sh"

if [ "$fail" = 0 ]; then echo "all dev-env tests passed"; fi
exit "$fail"
