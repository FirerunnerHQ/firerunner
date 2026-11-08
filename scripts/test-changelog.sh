#!/usr/bin/env bash
# test-changelog.sh — tests for scripts/changelog.py, each in a throwaway tree
# holding a copy of the script, a CHANGELOG.md and changelog.d/. Needs bash
# and python3.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0
check() { # check <name> <command...>
	local name="$1"
	shift
	if "$@"; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}
# shellcheck disable=SC2329 # called through check
not() { ! "$@"; }

# tree <dir>: a repository-shaped copy with one release and some unreleased lines.
tree() {
	mkdir -p "$1/scripts" "$1/changelog.d"
	cp "$ROOT/scripts/changelog.py" "$1/scripts/"
	cat >"$1/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

### Fixed

- An old-style line, still in CHANGELOG.md.

## [0.1.0] - 2026-01-01

### Added

- The first release.

[Unreleased]: https://github.com/FirerunnerHQ/firerunner/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.0
EOF
}
cl() { python3 -I "$T/scripts/changelog.py" "$@"; }
# shellcheck disable=SC2329 # called through check
quiet() { "$@" >/dev/null 2>&1; }

T="$TMP/ok"
tree "$T"
printf '### Fixed\n\n- A fix,\n  over two lines.\n' >"$T/changelog.d/fix.md"
printf '### Added\n\n- A feature.\n\n### Security\n\n- A guard.\n' >"$T/changelog.d/feature.md"
check "check accepts valid fragments" cl check
out="$(cl show)"
check "show merges CHANGELOG.md's lines and the fragments, by section" test "$out" = "### Added

- A feature.

### Fixed

- An old-style line, still in CHANGELOG.md.
- A fix,
  over two lines.

### Security

- A guard."
check "show --json lists the fragments" grep -q '"changelog.d/feature.md"' <<<"$(cl show --json)"
cl release 0.2.0 2026-02-02 >/dev/null
check "release writes the version under an empty Unreleased" \
	grep -qz '## \[Unreleased\]\n\n## \[0.2.0\] - 2026-02-02\n\n### Added\n\n- A feature.' "$T/CHANGELOG.md"
check "release keeps the earlier release" grep -q '## \[0.1.0\] - 2026-01-01' "$T/CHANGELOG.md"
check "release updates the compare links" grep -qz 'compare/v0.2.0...HEAD\n\[0.2.0\]: .*releases/tag/v0.2.0\n\[0.1.0\]' "$T/CHANGELOG.md"
check "release deletes the fragments it folded in" test ! -e "$T/changelog.d/fix.md" -a ! -e "$T/changelog.d/feature.md"
check "show after release has nothing" test "$(cl show)" = "(no unreleased changes)"
check "a second release of the same version is refused" not quiet cl release 0.2.0

T="$TMP/bad"
tree "$T"
printf '### Fixes\n\n- Wrong section.\n' >"$T/changelog.d/a.md"
check "check rejects an unknown section" not quiet cl check
printf 'No heading.\n' >"$T/changelog.d/a.md"
check "check rejects text before a heading" not quiet cl check
printf '### Fixed\n\nNot a bullet.\n' >"$T/changelog.d/a.md"
check "check rejects a line that is not an entry" not quiet cl check
out="$(cl check 2>&1)"
check "check names the file and line" grep -q 'changelog.d/a.md:3' <<<"$out"
rm "$T/changelog.d/a.md"
check "check passes with no fragments" quiet cl check
check "release with nothing unreleased beyond CHANGELOG.md still works" quiet cl release 0.3.0

T="$TMP/usage"
tree "$T"
cl release 1.2 >/dev/null 2>&1
check "a malformed version is a usage error" test "$?" = 2
cl frobnicate >/dev/null 2>&1
check "an unknown command is a usage error" test "$?" = 2
check "--help exits 0" quiet cl --help

if [ "$fail" = 0 ]; then echo "all changelog tests passed"; fi
exit "$fail"
