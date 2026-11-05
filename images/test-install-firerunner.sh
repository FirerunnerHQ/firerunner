#!/usr/bin/env bash
# Unit tests for ec2/install-firerunner.sh, called as provision.sh calls it: in
# an `||` list under set -e, which is what makes a missing check silent. A fake
# curl serves files from $STATE/remote; sha256sum is real where there is one.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
export STATE="$TMP/state"
fails=0

cat >"$TMP/bin/curl" <<'FAKE'
#!/bin/sh
# curl -fsSL --retry 3 -o FILE URL
out="$5"
for last; do :; done
echo "$last" >> "$STATE/urls"
file="$STATE/remote/${last##*/}"
[ -f "$file" ] || exit 22
cp "$file" "$out"
FAKE
chmod +x "$TMP/bin/curl"
if ! command -v sha256sum >/dev/null; then
	printf '#!/bin/sh\nexec shasum -a 256 "$@"\n' >"$TMP/bin/sha256sum"
	chmod +x "$TMP/bin/sha256sum"
fi

# A binary with the subcommand, and one too old for it.
# shellcheck disable=SC2016 # the fake evaluates this
good='#!/bin/sh
[ "$1" = git-proxy ]'
old='#!/bin/sh
exit 2'

fresh() {
	rm -rf "$STATE"
	mkdir -p "$STATE/remote"
	export RUNNER_ARCH=x64 FR_VERSION="" FR_URL="" FR_SHA256=""
	export FIRERUNNER_BIN="$STATE/firerunner" FIRERUNNER_RELEASES="https://releases.test"
	printf '%s\n' "$good" >"$STATE/remote/firerunner_linux_amd64"
}
sums() {
	(cd "$STATE/remote" && sha256sum firerunner_linux_* >SHA256SUMS)
}
# run: as provision.sh does, so errexit is off inside the function.
run() {
	(
		set -euo pipefail
		PATH="$TMP/bin:$PATH"
		# shellcheck source=ec2/install-firerunner.sh disable=SC1091
		. "$HERE/ec2/install-firerunner.sh"
		install_firerunner || echo "not installed" >"$STATE/result"
	)
}
pass() { # DESC CMD...
	local d="$1"
	shift
	if "$@"; then echo "ok   $d"; else
		echo "FAIL $d"
		fails=$((fails + 1))
	fi
}
installed() { [ -x "$FIRERUNNER_BIN" ]; }
absent() { [ ! -e "$FIRERUNNER_BIN" ] && [ -f "$STATE/result" ]; }

fresh
sums
run
pass "a verified release installs" installed
pass "the latest release by default" grep -q 'https://releases.test/latest/download/firerunner_linux_amd64' "$STATE/urls"
pass "no failure reported" [ ! -f "$STATE/result" ]

fresh
sums
FR_VERSION=v0.1.1 run
pass "a named version: asks for it" grep -q 'releases.test/download/v0.1.1/SHA256SUMS' "$STATE/urls"

fresh
export RUNNER_ARCH=arm64
cp "$STATE/remote/firerunner_linux_amd64" "$STATE/remote/firerunner_linux_arm64"
sums
RUNNER_ARCH=arm64 run
pass "arm64 takes its own binary" grep -q 'firerunner_linux_arm64$' "$STATE/urls"

# The point of the check: a tampered binary must not reach the image.
fresh
sums
printf '%s\n# tampered\n' "$good" >"$STATE/remote/firerunner_linux_amd64"
run
pass "tampered release: not installed" absent

fresh
run
pass "no SHA256SUMS: not installed" absent

fresh
sums
: >"$STATE/remote/SHA256SUMS"
run
pass "binary not in SHA256SUMS: not installed" absent

fresh
sums
rm "$STATE/remote/firerunner_linux_amd64"
run
pass "no binary: not installed" absent

fresh
printf '%s\n' "$old" >"$STATE/remote/firerunner_linux_amd64"
sums
run
pass "a release without git-proxy: not installed" absent

fresh
right="$(sha256sum "$STATE/remote/firerunner_linux_amd64" | cut -d' ' -f1)"
FR_URL=https://x.test/firerunner_linux_amd64 FR_SHA256="$right" run
pass "a URL with its hash: installs" installed

fresh
FR_URL=https://x.test/firerunner_linux_amd64 FR_SHA256="$(printf '0%.0s' {1..64})" run
pass "a URL with the wrong hash: not installed" absent

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
