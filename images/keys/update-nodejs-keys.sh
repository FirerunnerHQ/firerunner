#!/usr/bin/env bash
# update-nodejs-keys.sh — refresh the Node.js release keys the image builders
# check SHASUMS256.txt.sig against (lib/verify.sh).
#
# Usage: images/keys/update-nodejs-keys.sh [COMMIT]
#   COMMIT  a nodejs/release-keys commit (default: its main branch now)
#
# It fetches nodejs/release-keys at COMMIT with git, takes the key of every
# fingerprint in its keys.list, and refuses unless each key file holds exactly
# that key and the list matches the release keys nodejs/node's README names,
# a second source in a different repository. It then writes, beside itself:
#   nodejs-release-keys.asc   the keys, ASCII-armored, in keys.list order
#   nodejs-release-keys.list  the source commit, and one fingerprint and name
#                             per key, so a refresh reviews as a short diff
# Run it when a build fails because a release is signed by a key it lacks
# (a new releaser), then review the diff of the .list file.
# Needs git, gpg and curl.
set -euo pipefail

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }
die() {
	echo "update-nodejs-keys: $*" >&2
	exit 1
}
case "${1:-}" in -h | --help)
	usage
	exit 0
	;;
esac
[ $# -le 1 ] || die "usage: $0 [COMMIT]"
ref="${1:-main}"
for t in git gpg curl; do command -v "$t" >/dev/null || die "needs $t"; done

here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"

repo="$work/release-keys"
git init -q "$repo"
git -C "$repo" fetch -q --depth 1 https://github.com/nodejs/release-keys "$ref"
git -C "$repo" -c advice.detachedHead=false checkout -q FETCH_HEAD
commit="$(git -C "$repo" rev-parse HEAD)"

# The fingerprints, in list order: the first word of each non-empty line.
awk 'NF { print $1 }' "$repo/keys.list" >"$work/listed"
[ -s "$work/listed" ] || die "keys.list at $commit lists no key"

# nodejs/node's README names the same keys under "Release keys", each in
# backticks.
# shellcheck disable=SC2016 # literal backticks, not a command substitution
curl -fsSL --retry 3 https://raw.githubusercontent.com/nodejs/node/main/README.md |
	awk '/^#+ *Release keys/ { on = 1; next } on && /^#+ / { exit } on' |
	grep -oE '`[0-9A-F]{40}`' | tr -d '`' | sort -u >"$work/readme"
[ -s "$work/readme" ] || die "found no release keys in nodejs/node's README"
sort -u "$work/listed" >"$work/listed.sorted"
if ! diff -u "$work/readme" "$work/listed.sorted" >"$work/diff"; then
	cat "$work/diff" >&2
	die "release-keys $commit and nodejs/node's README name different keys (- README, + keys.list)"
fi

: >"$work/keys.asc"
{
	echo "# Node.js release keys, from nodejs/release-keys $commit."
	echo "# Written by update-nodejs-keys.sh; checked against nodejs/node's README."
} >"$work/keys.list"
while read -r fpr; do
	f="$repo/keys/$fpr.asc"
	[ -f "$f" ] || die "keys.list names $fpr, but keys/$fpr.asc is missing"
	got="$(gpg --show-keys --with-colons "$f" 2>/dev/null | awk -F: '$1 == "pub" { p = 1; next } p && $1 == "fpr" { print $10; p = 0 }')"
	[ "$got" = "$fpr" ] || die "keys/$fpr.asc holds ${got:-no key} instead"
	name="$(gpg --show-keys --with-colons "$f" 2>/dev/null | awk -F: '$1 == "uid" { print $10; exit }')"
	cat "$f" >>"$work/keys.asc"
	echo "$fpr $name" >>"$work/keys.list"
done <"$work/listed"

install -m 0644 "$work/keys.asc" "$here/nodejs-release-keys.asc"
install -m 0644 "$work/keys.list" "$here/nodejs-release-keys.list"
echo "update-nodejs-keys: $(wc -l <"$work/listed" | tr -d ' ') keys from nodejs/release-keys $commit"
