#!/usr/bin/env bash
# Tests that the image builders check what they download before they use it.
# Each builder runs end to end on any host: the commands that need root, Linux
# or the network are fakes. The fake curl serves files by URL from a routes
# table, so a test can hand out a tampered archive; tar and sha256sum are real.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
# Node's SHASUMS256.txt is signed: a test release key and a stranger's sign the
# fixtures. GNUPGHOME stays short, since gpg-agent's socket lives in it.
for t in gpg gpgv gpgconf; do
	command -v "$t" >/dev/null || {
		echo "test-verify-downloads: needs $t (gnupg)" >&2
		exit 1
	}
done
GPG_RELEASE="$(mktemp -d /tmp/frgpg.XXXXXX)"
GPG_STRANGER="$(mktemp -d /tmp/frgpg.XXXXXX)"
cleanup() {
	for h in "$GPG_RELEASE" "$GPG_STRANGER"; do gpgconf --homedir "$h" --kill gpg-agent 2>/dev/null || true; done
	rm -rf "$TMP" "$GPG_RELEASE" "$GPG_STRANGER"
}
trap cleanup EXIT
BIN="$TMP/bin"
REMOTE="$TMP/remote"
mkdir -p "$BIN" "$REMOTE"
export REMOTE
fails=0

# curl [-o FILE] [-w FORMAT] [-H HEADER] ... URL; the URL is the last argument.
cat >"$BIN/curl" <<'FAKE'
#!/bin/sh
out=""
while [ $# -gt 1 ]; do
	case "$1" in
	-o) out="$2"; shift ;;
	-w | -H | --retry) shift ;;
	esac
	shift
done
echo "$1" >>"$STATE/urls"
file="$(awk -v u="$1" '$1 == u { print $2; exit }' "$REMOTE/routes")"
[ -n "$file" ] && [ -f "$file" ] || exit 22
if [ -n "$out" ]; then cp "$file" "$out"; else cat "$file"; fi
FAKE
# Linux, root, and no real disks.
printf '#!/bin/sh\necho Linux\n' >"$BIN/uname"
printf '#!/bin/sh\necho 0\n' >"$BIN/id"
for t in truncate mount umount e2label; do
	# shellcheck disable=SC2016 # the fake expands it
	printf '#!/bin/sh\necho "%s $*" >>"$STATE/calls"\n' "$t" >"$BIN/$t"
done
printf '#!/bin/sh\nexit 1\n' >"$BIN/mountpoint"
# mkfs.ext4 [-d DIR] ...: keeps the record of downloads DIR holds, and its
# listing.
cat >"$BIN/mkfs.ext4" <<'FAKE'
#!/bin/sh
echo "mkfs.ext4 $*" >>"$STATE/calls"
while [ $# -gt 1 ]; do
	if [ "$1" = -d ]; then
		for f in "$2/etc/firerunner/downloads.sha256" "$2/downloads.sha256"; do
			[ ! -f "$f" ] || cp "$f" "$STATE/"
		done
		(cd "$2" && find . -type f) >"$STATE/drive"
	fi
	shift
done
FAKE
# docker: build keeps a copy of its context; export streams a bare tree; run
# lists what is mounted at /dl.
cat >"$BIN/docker" <<'FAKE'
#!/bin/sh
echo "docker $*" >>"$STATE/calls"
case "$1" in
build)
	for ctx; do :; done
	cp -R "$ctx" "$STATE/ctx"
	;;
export)
	mkdir -p "$STATE/rootfs/etc"
	tar -cf - -C "$STATE/rootfs" .
	;;
run)
	for a; do
		case "$a" in *:/dl:ro) ls "${a%:/dl:ro}" >"$STATE/python-dl" ;; esac
	done
	;;
esac
FAKE
# debootstrap ... SUITE TARGET MIRROR: the directories the builder writes into.
cat >"$BIN/debootstrap" <<'FAKE'
#!/bin/sh
mkdir -p "$4/etc/systemd/system" "$4/usr/local/bin" "$4/var/lib/apt/lists" "$4/var/cache/apt/archives"
FAKE
# chroot ROOT CMD...: records what ran, and what the root held at the time.
cat >"$BIN/chroot" <<'FAKE'
#!/bin/sh
root="$1"
shift
echo "$*" >>"$STATE/chroot"
[ ! -f "$root/opt/runner/bin/installdependencies.sh" ] || touch "$STATE/runner-installed"
[ ! -f "$root/usr/local/bin/node" ] || touch "$STATE/node-installed"
[ ! -f "$root/etc/firerunner/downloads.sha256" ] || cp "$root/etc/firerunner/downloads.sha256" "$STATE/"
FAKE
# The fakes are POSIX sh: run them with dash where there is one, as Ubuntu does.
if SH="$(command -v dash)"; then
	for f in "$BIN"/*; do
		sed -i.bak "1s|^#!/bin/sh\$|#!$SH|" "$f"
		rm -f "$f.bak"
	done
fi
chmod +x "$BIN"/*
sha() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'
}

# archive NAME DIR-IN-ARCHIVE FILE...: a .tar.gz in $REMOTE holding FILEs.
archive() {
	local name="$1" top="$2" src
	shift 2
	src="$(mktemp -d "$TMP/src.XXXXXX")"
	mkdir -p "$src/$top"
	for f; do
		mkdir -p "$src/$top/$(dirname "$f")"
		printf '#!/bin/sh\necho %s\n' "$name" >"$src/$top/$f"
		chmod +x "$src/$top/$f"
	done
	tar -czf "$REMOTE/$name" -C "$src" "$top"
}
serve() { echo "$1 $2" >>"$REMOTE/routes"; }

RV=2.400.0
RUNNER_TGZ="actions-runner-linux-x64-$RV.tar.gz"
RUNNER_URL="https://github.com/actions/runner/releases/download/v$RV/$RUNNER_TGZ"
archive "$RUNNER_TGZ" . bin/installdependencies.sh
archive evil-runner.tar.gz . bin/installdependencies.sh bin/backdoor
RUNNER_SHA="$(sha "$REMOTE/$RUNNER_TGZ")"
NV=22.11.0
NODE_TGZ="node-v$NV-linux-x64.tar.gz"
NODE_URL="https://nodejs.org/dist/v$NV/$NODE_TGZ"
archive "$NODE_TGZ" "node-v$NV-linux-x64" bin/node
archive evil-node.tar.gz "node-v$NV-linux-x64" bin/node bin/backdoor
NODE_SHA="$(sha "$REMOTE/$NODE_TGZ")"

# release FILE SHA: actions/runner's release, its notes carrying SHA for x64.
release() {
	printf '{\n  "tag_name": "v%s",\n  "body": "## Linux x64\\r\\n- %s <!-- BEGIN SHA linux-x64 -->%s<!-- END SHA linux-x64 -->\\r\\n- arm64 <!-- BEGIN SHA linux-arm64 -->%s<!-- END SHA linux-arm64 -->"\n}\n' \
		"$RV" "$RUNNER_TGZ" "$2" "$(printf '%064d' 7)" >"$REMOTE/$1"
}
release release.json "$RUNNER_SHA"
release release-nosha.json ""
printf '%s  %s\n%s  node-v%s-darwin-arm64.tar.gz\n' "$NODE_SHA" "$NODE_TGZ" "$(printf '%064d' 1)" "$NV" >"$REMOTE/SHASUMS256.txt"
printf '%s  node-v%s-darwin-arm64.tar.gz\n' "$(printf '%064d' 1)" "$NV" >"$REMOTE/SHASUMS256-other.txt"
# A release key the builders trust (NODE_KEYRING), and a stranger's.
for h in "$GPG_RELEASE" "$GPG_STRANGER"; do
	chmod 700 "$h"
	gpg --homedir "$h" --batch --quiet --passphrase '' --quick-gen-key "test <$(basename "$h")@example.com>" ed25519 sign never 2>/dev/null
done
gpg --homedir "$GPG_RELEASE" --batch --armor --export >"$TMP/node-keys.asc"
RELEASE_FPR="$(gpg --homedir "$GPG_RELEASE" --batch --with-colons --list-keys 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }')"
export NODE_KEYRING="$TMP/node-keys.asc"
sign() { # HOME FILE OUT: OUT is FILE's detached signature by HOME's key.
	gpg --homedir "$1" --batch --quiet --yes --detach-sign -o "$3" "$2"
}
sign "$GPG_RELEASE" "$REMOTE/SHASUMS256.txt" "$REMOTE/SHASUMS256.txt.sig"
sign "$GPG_RELEASE" "$REMOTE/SHASUMS256-other.txt" "$REMOTE/SHASUMS256-other.txt.sig"
sign "$GPG_STRANGER" "$REMOTE/SHASUMS256.txt" "$REMOTE/SHASUMS256-stranger.txt.sig"
# A SHASUMS256.txt that lists a tampered archive, under the real signature.
printf '%s  %s\n' "$(sha "$REMOTE/evil-node.tar.gz")" "$NODE_TGZ" >"$REMOTE/SHASUMS256-forged.txt"

printf 'not the real package\n' >"$REMOTE/pmc.deb"
PMC_SHA="$(sha "$REMOTE/pmc.deb")"
PMC_URL=https://packages.microsoft.com/config/ubuntu/24.04/packages-microsoft-prod.deb

# actions/runner-images, served by real git from a local directory: commit
# RI_A, then RI_B, to which the tag has since moved.
export GITHUB_GIT="file://$TMP/git" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
RI="$TMP/git/actions/runner-images"
mkdir -p "$RI/images/ubuntu/scripts/helpers" "$RI/images/ubuntu/scripts/tests" \
	"$RI/images/ubuntu/scripts/build" "$RI/images/ubuntu/toolsets"
echo '{}' >"$RI/images/ubuntu/toolsets/toolset-2404.json"
: >"$RI/images/ubuntu/scripts/helpers/install.sh"
: >"$RI/images/ubuntu/scripts/tests/none.sh"
ri_commit() {
	echo "$1" >"$RI/images/ubuntu/scripts/build/which.sh"
	git -C "$RI" add -A
	git -C "$RI" -c user.name=t -c user.email=t@example.com commit -q -m "$1"
	git -C "$RI" rev-parse HEAD
}
git -C "$RI" init -q
RI_A="$(ri_commit A)"
RI_B="$(ri_commit B)"
printf '%s' "$RI_B" >"$REMOTE/ri-tag"
printf '{"message": "No commit found for SHA: ubuntu24/bad"}' >"$REMOTE/ri-bad"

# The tool-cache drive's sources.
GV=1.26.7
GO_TGZ="go$GV.linux-amd64.tar.gz"
GO_URL="https://go.dev/dl/$GO_TGZ"
GO_INDEX="https://go.dev/dl/?mode=json&include=all"
archive "$GO_TGZ" go bin/go
archive evil-go.tar.gz go bin/go bin/backdoor
GO_SHA="$(sha "$REMOTE/$GO_TGZ")"
printf '[{"version": "go%s", "files": [{"filename": "go%s.src.tar.gz", "sha256": "%s"}, {"filename": "%s", "sha256": "%s"}]}]\n' \
	"$GV" "$GV" "$(printf '%064d' 5)" "$GO_TGZ" "$GO_SHA" >"$REMOTE/go.json"
echo '[]' >"$REMOTE/go-empty.json"
PY_TAG=3.12.9-123
PY_TGZ="python-3.12.9-linux-24.04-x64.tar.gz"
PY_URL="https://github.com/actions/python-versions/releases/download/$PY_TAG/$PY_TGZ"
PY_API="https://api.github.com/repos/actions/python-versions/releases/tags/$PY_TAG"
PY_MANIFEST=https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json
archive "$PY_TGZ" . setup.sh
archive evil-python.tar.gz . setup.sh backdoor
PY_SHA="$(sha "$REMOTE/$PY_TGZ")"
py_manifest() { # FILE URL
	printf '[{"version": "3.12.9", "files": [{"platform": "linux", "arch": "x64", "platform_version": "24.04", "download_url": "%s"}, {"platform": "darwin", "arch": "x64", "platform_version": "14", "download_url": "https://example.com/other"}]}, {"version": "3.13.0", "files": []}]\n' "$2" >"$REMOTE/$1"
}
py_manifest python.json "$PY_URL"
py_manifest python-outside.json "https://example.com/$PY_TGZ"
asset() { # FILE NAME DIGEST
	printf '{"tag_name": "x", "assets": [{"name": "other.tar.gz", "digest": "sha256:%s"}, {"name": "%s", "digest": %s}]}\n' \
		"$(printf '%064d' 6)" "$2" "$3" >"$REMOTE/$1"
}
asset python-release.json "$PY_TGZ" "\"sha256:$PY_SHA\""
asset python-release-old.json "$PY_TGZ" null
CQ=2.27.4
CQ_URL="https://github.com/github/codeql-action/releases/download/codeql-bundle-v$CQ/codeql-bundle-linux64.tar.gz"
CQ_API="https://api.github.com/repos/github/codeql-action/releases/tags/codeql-bundle-v$CQ"
archive codeql.tar.gz codeql codeql
archive evil-codeql.tar.gz codeql codeql backdoor
CQ_SHA="$(sha "$REMOTE/codeql.tar.gz")"
asset codeql-release.json codeql-bundle-linux64.tar.gz "\"sha256:$CQ_SHA\""

# routes: the honest upstream.
routes() {
	: >"$REMOTE/routes"
	serve https://api.github.com/repos/actions/runner/releases/latest "$REMOTE/release.json"
	serve "https://api.github.com/repos/actions/runner/releases/tags/v$RV" "$REMOTE/release.json"
	serve "$RUNNER_URL" "$REMOTE/$RUNNER_TGZ"
	serve "https://nodejs.org/dist/v$NV/SHASUMS256.txt" "$REMOTE/SHASUMS256.txt"
	serve "https://nodejs.org/dist/v$NV/SHASUMS256.txt.sig" "$REMOTE/SHASUMS256.txt.sig"
	serve "$NODE_URL" "$REMOTE/$NODE_TGZ"
	serve "$PMC_URL" "$REMOTE/pmc.deb"
	serve https://api.github.com/repos/actions/runner-images/commits/ubuntu24/test "$REMOTE/ri-tag"
	serve https://api.github.com/repos/actions/runner-images/commits/ubuntu24/bad "$REMOTE/ri-bad"
	serve "$GO_INDEX" "$REMOTE/go.json"
	serve "$GO_URL" "$REMOTE/$GO_TGZ"
	serve "$PY_MANIFEST" "$REMOTE/python.json"
	serve "$PY_API" "$REMOTE/python-release.json"
	serve "$PY_URL" "$REMOTE/$PY_TGZ"
	serve "$CQ_API" "$REMOTE/codeql-release.json"
	serve "$CQ_URL" "$REMOTE/codeql.tar.gz"
}
# reroute URL FILE: serve FILE at URL instead.
reroute() {
	awk -v u="$1" '$1 != u' "$REMOTE/routes" >"$REMOTE/routes.new"
	mv "$REMOTE/routes.new" "$REMOTE/routes"
	serve "$1" "$2"
}

# run NAME SCRIPT ARGS...: leaves rc, out, err, urls and the fakes' records in
# $STATE.
run() {
	STATE="$TMP/state-$1"
	mkdir -p "$STATE"
	: >"$STATE/urls"
	export STATE
	local script="$2"
	shift 2
	if PATH="$BIN:$PATH" "$script" "$@" >"$STATE/out" 2>"$STATE/err"; then
		echo 0 >"$STATE/rc"
	else
		echo $? >"$STATE/rc"
	fi
}
expect() { # DESC CMD...
	local d="$1"
	shift
	if "$@"; then echo "ok   $d"; else
		echo "FAIL $d"
		sed 's/^/     /' "$STATE/err" | tail -n 5
		fails=$((fails + 1))
	fi
}
passed() { [ "$(cat "$STATE/rc")" = 0 ]; }
failed() { [ "$(cat "$STATE/rc")" != 0 ]; }
has() { grep -qF -- "$2" "$STATE/$1"; }
lacks() { ! grep -qF -- "$2" "$STATE/$1" 2>/dev/null; }
exists() { [ -e "$STATE/$1" ]; }
absent() { [ ! -e "$STATE/$1" ]; }

echo "== lib/verify.sh"
routes
STATE="$TMP/state-lib"
mkdir -p "$STATE"
: >"$STATE/urls"
export STATE
if (
	PATH="$BIN:$PATH"
	# shellcheck source=lib/verify.sh disable=SC1091
	. "$HERE/lib/verify.sh"
	fetch_checked "$RUNNER_URL" "$STATE/got" "" runner
) 2>"$STATE/err"; then echo 0 >"$STATE/rc"; else echo 1 >"$STATE/rc"; fi
expect "no checksum: fails" failed
expect "no checksum: downloads nothing" lacks urls "$RUNNER_URL"

ROOTFS="$HERE/build-rootfs.sh"
OUT="$TMP/out/golden.ext4"

echo "== build-rootfs.sh"
routes
run rootfs-latest "$ROOTFS" --out "$OUT"
expect "latest: builds" passed
expect "latest: resolves the version once" has urls https://api.github.com/repos/actions/runner/releases/latest
expect "latest: installs the runner" exists runner-installed
expect "latest: prints the version and checksum it used" has out "actions/runner $RV linux-x64 sha256 $RUNNER_SHA"
expect "latest: the image records the checksum" has downloads.sha256 "$RUNNER_SHA  $RUNNER_TGZ"

routes
reroute "$RUNNER_URL" "$REMOTE/evil-runner.tar.gz"
run rootfs-tampered "$ROOTFS" --out "$OUT"
expect "tampered runner: fails" failed
expect "tampered runner: says why" has err "$RUNNER_TGZ: SHA-256 mismatch: expected $RUNNER_SHA"
expect "tampered runner: never unpacked" absent runner-installed
expect "tampered runner: nothing runs in the image" lacks chroot installdependencies

routes
reroute "https://api.github.com/repos/actions/runner/releases/tags/v$RV" "$REMOTE/release-nosha.json"
run rootfs-nosha "$ROOTFS" --out "$OUT" --runner-version "$RV"
expect "notes without a checksum: fails" failed
expect "notes without a checksum: says so" has err "carry no SHA-256 for linux-x64"
expect "notes without a checksum: fails before building" lacks calls mkfs.ext4
expect "notes without a checksum: downloads nothing" lacks urls releases/download

routes
run rootfs-pinned "$ROOTFS" --out "$OUT" --runner-version "$RV" --runner-sha256 "$RUNNER_SHA"
expect "pinned: builds" passed
expect "pinned: reads no release notes" lacks urls api.github.com
expect "pinned: installs the runner" exists runner-installed

run rootfs-pinned-wrong "$ROOTFS" --out "$OUT" --runner-version "$RV" --runner-sha256 "$(printf '%064d' 3)"
expect "wrong pin: fails" failed
expect "wrong pin: never unpacked" absent runner-installed

run rootfs-pin-no-version "$ROOTFS" --out "$OUT" --runner-sha256 "$RUNNER_SHA"
expect "a pin without a version: refused" has err "--runner-sha256 needs --runner-version"

routes
run rootfs-node "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "node: builds" passed
expect "node: checks against SHASUMS256.txt" has urls "https://nodejs.org/dist/v$NV/SHASUMS256.txt"
expect "node: checks its signature" has urls "https://nodejs.org/dist/v$NV/SHASUMS256.txt.sig"
expect "node: uses the SHASUMS256.txt it checked, read once" \
	test "$(grep -cxF "https://nodejs.org/dist/v$NV/SHASUMS256.txt" "$STATE/urls")" = 1
expect "node: names the release key that signed it" has err "SHASUMS256.txt signed by release key $RELEASE_FPR"
expect "node: installs node" exists node-installed
expect "node: the image records both checksums" has downloads.sha256 "$NODE_SHA  $NODE_TGZ"

routes
reroute "$NODE_URL" "$REMOTE/evil-node.tar.gz"
run rootfs-node-tampered "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "tampered node: fails" failed
expect "tampered node: says why" has err "$NODE_TGZ: SHA-256 mismatch"
expect "tampered node: never unpacked" absent node-installed

routes
reroute "https://nodejs.org/dist/v$NV/SHASUMS256.txt" "$REMOTE/SHASUMS256-other.txt"
reroute "https://nodejs.org/dist/v$NV/SHASUMS256.txt.sig" "$REMOTE/SHASUMS256-other.txt.sig"
run rootfs-node-unlisted "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "node missing from SHASUMS256.txt: fails before building" failed
expect "node missing from SHASUMS256.txt: says so" has err "lists no $NODE_TGZ"
expect "node missing from SHASUMS256.txt: no image" lacks calls mkfs.ext4

# SHASUMS256.txt is believed only under a release key's signature.
routes
reroute "https://nodejs.org/dist/v$NV/SHASUMS256.txt" "$REMOTE/SHASUMS256-forged.txt"
reroute "$NODE_URL" "$REMOTE/evil-node.tar.gz"
run rootfs-node-forged "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "forged SHASUMS256.txt: fails before building" failed
expect "forged SHASUMS256.txt: says why" has err "is not signed by a Node.js release key"
expect "forged SHASUMS256.txt: no image" lacks calls mkfs.ext4
expect "forged SHASUMS256.txt: the archive is never fetched" lacks urls "$NODE_URL"

routes
reroute "https://nodejs.org/dist/v$NV/SHASUMS256.txt.sig" "$REMOTE/SHASUMS256-stranger.txt.sig"
run rootfs-node-stranger "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "signed by a stranger: fails" has err "is not signed by a Node.js release key"
expect "signed by a stranger: no image" lacks calls mkfs.ext4

routes
reroute "https://nodejs.org/dist/v$NV/SHASUMS256.txt.sig" "$REMOTE/missing"
run rootfs-node-unsigned "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "no signature: fails" has err "could not read https://nodejs.org/dist/v$NV/SHASUMS256.txt and its .sig"
expect "no signature: no image" lacks calls mkfs.ext4

routes
run rootfs-node-pinned "$ROOTFS" --out "$OUT" --tier firerunner-node --node-sha256 "$NODE_SHA"
expect "node pinned: builds without SHASUMS256.txt" passed
expect "node pinned: installs node" exists node-installed

UBUNTU="$HERE/build-ubuntu-rootfs.sh"
OUT="$TMP/out/ubuntu.ext4"
same() { cmp -s "$STATE/$1" "$2"; }
ri_at() { [ "$(cat "$STATE/ctx/imagegeneration/installers/which.sh")" = "$1" ]; }

echo "== build-ubuntu-rootfs.sh"
routes
run ubuntu-minimal "$UBUNTU" --out "$OUT" --toolset minimal
expect "minimal: builds" passed
expect "minimal: the build gets the checked runner" same ctx/actions-runner.tar.gz "$REMOTE/$RUNNER_TGZ"
expect "minimal: the build gets the checked node" same ctx/node.tar.gz "$REMOTE/$NODE_TGZ"
expect "minimal: the Dockerfile downloads no runner" lacks ctx/Dockerfile releases/download
expect "minimal: the Dockerfile downloads no node" lacks ctx/Dockerfile nodejs.org
expect "minimal: prints the version and checksum it used" has out "actions/runner $RV linux-x64 sha256 $RUNNER_SHA"
expect "minimal: the image records the runner" has downloads.sha256 "$RUNNER_SHA  $RUNNER_TGZ"
expect "minimal: the image records node" has downloads.sha256 "$NODE_SHA  $NODE_TGZ"

routes
reroute "$RUNNER_URL" "$REMOTE/evil-runner.tar.gz"
run ubuntu-tampered-runner "$UBUNTU" --out "$OUT" --toolset minimal
expect "tampered runner: fails" failed
expect "tampered runner: says why" has err "$RUNNER_TGZ: SHA-256 mismatch"
expect "tampered runner: nothing is built" lacks calls "docker build"

routes
reroute "$NODE_URL" "$REMOTE/evil-node.tar.gz"
run ubuntu-tampered-node "$UBUNTU" --out "$OUT" --toolset minimal
expect "tampered node: fails" failed
expect "tampered node: says why" has err "$NODE_TGZ: SHA-256 mismatch"
expect "tampered node: nothing is built" lacks calls "docker build"

routes
run ubuntu-pinned "$UBUNTU" --out "$OUT" --toolset minimal \
	--runner-version "$RV" --runner-sha256 "$RUNNER_SHA" --node-sha256 "$NODE_SHA"
expect "pinned: builds" passed
expect "pinned: reads no release notes" lacks urls api.github.com
expect "pinned: reads no SHASUMS256.txt" lacks urls SHASUMS256.txt

routes
run ubuntu-full-tag "$UBUNTU" --out "$OUT" --toolset full --runner-images-ref ubuntu24/test --pmc-sha256 "$PMC_SHA"
expect "full, tag: builds" passed
expect "full, tag: resolves the tag once and says how to pin it" has out "resolved runner-images ubuntu24/test to commit $RI_B; pass --runner-images-commit $RI_B"
expect "full, tag: vendors that commit" ri_at B
expect "full, tag: the build gets the checked package" same ctx/packages-microsoft-prod.deb "$REMOTE/pmc.deb"
expect "full, tag: the Dockerfile downloads no package" lacks ctx/Dockerfile packages.microsoft.com
expect "full, tag: the image records the commit" has downloads.sha256 "$RI_B  actions/runner-images (git commit)"
expect "full, tag: the image records the package" has downloads.sha256 "$PMC_SHA  packages-microsoft-prod.deb"

run ubuntu-full-commit "$UBUNTU" --out "$OUT" --toolset full --runner-images-ref ubuntu24/test \
	--runner-images-commit "$RI_A" --pmc-sha256 "$PMC_SHA"
expect "full, commit: builds" passed
expect "full, commit: vendors the pinned commit, not the moved tag" ri_at A
expect "full, commit: does not resolve the tag" lacks urls runner-images/commits

run ubuntu-full-default "$UBUNTU" --out "$OUT" --toolset full --pmc-sha256 "$PMC_SHA"
expect "full, default ref: fetches its pinned commit" has err "could not fetch actions/runner-images at commit 3b5f596ffecb076aa5f3c3ded95b145f6daeb016"
expect "full, default ref: does not resolve the tag" lacks urls runner-images/commits

run ubuntu-full-unknown "$UBUNTU" --out "$OUT" --toolset full --runner-images-commit "$(printf '%040d' 9)" --pmc-sha256 "$PMC_SHA"
expect "full, unknown commit: fails at the fetch" has err "could not fetch actions/runner-images at commit $(printf '%040d' 9)"
expect "full, unknown commit: goes no further" lacks err layout
expect "full, unknown commit: nothing is built" lacks calls "docker build"

run ubuntu-full-branch "$UBUNTU" --out "$OUT" --toolset full --runner-images-commit "$(git -C "$RI" branch --show-current)" --pmc-sha256 "$PMC_SHA"
expect "full, a branch as the commit: refused" has err "is not a commit SHA"
expect "full, a branch as the commit: nothing is built" lacks calls "docker build"

run ubuntu-full-badref "$UBUNTU" --out "$OUT" --toolset full --runner-images-ref ubuntu24/bad --pmc-sha256 "$PMC_SHA"
expect "full, a tag the API cannot resolve: fails" has err "actions/runner-images ubuntu24/bad resolved to"
expect "full, a tag the API cannot resolve: fetches nothing" lacks err "could not fetch"

run ubuntu-pin-no-version "$UBUNTU" --out "$OUT" --runner-sha256 "$RUNNER_SHA"
expect "a pin without a version: refused" has err "--runner-sha256 needs --runner-version"

run ubuntu-full-pmc "$UBUNTU" --out "$OUT" --toolset full --runner-images-commit "$RI_A"
expect "full, package: fails against the pinned checksum" has err "packages-microsoft-prod.deb (ubuntu 24.04): SHA-256 mismatch: expected c13f01ac"
expect "full, package: nothing is built" lacks calls "docker build"

run ubuntu-full-pmc-tag "$UBUNTU" --out "$OUT" --toolset full --ubuntu-tag 22.04 --runner-images-commit "$RI_A"
expect "full, package for another release: needs a pin" has err "pass --pmc-sha256"

TOOLCACHE="$HERE/build-toolcache.sh"
OUT="$TMP/out/toolcache.ext4"

echo "== build-toolcache.sh"
routes
run tc-node-go "$TOOLCACHE" --out "$OUT" --node "$NV" --go "$GV"
expect "node and go: builds" passed
expect "node and go: node is on the drive" has drive "./node/$NV/x64/bin/node"
expect "node and go: go is on the drive" has drive "./go/$GV/x64/bin/go"
expect "node and go: go is checked against the download index" has urls "$GO_INDEX"
expect "node and go: the drive records node" has downloads.sha256 "$NODE_SHA  $NODE_TGZ"
expect "node and go: the drive records go" has downloads.sha256 "$GO_SHA  $GO_TGZ"
expect "node and go: no archive on the drive" lacks drive .tar.gz

for tool in node go; do
	routes
	if [ "$tool" = node ]; then reroute "$NODE_URL" "$REMOTE/evil-node.tar.gz"; else reroute "$GO_URL" "$REMOTE/evil-go.tar.gz"; fi
	run "tc-tampered-$tool" "$TOOLCACHE" --out "$OUT" --node "$NV" --go "$GV"
	expect "tampered $tool: fails" failed
	expect "tampered $tool: says why" has err ": SHA-256 mismatch"
	expect "tampered $tool: no drive" lacks calls mkfs.ext4
done

routes
reroute "$GO_INDEX" "$REMOTE/go-empty.json"
run tc-go-unlisted "$TOOLCACHE" --out "$OUT" --go "$GV"
expect "go missing from the index: fails" has err "lists no $GO_TGZ"
expect "go missing from the index: stops there" lacks err "no SHA-256 to check it against"
expect "go missing from the index: downloads nothing" lacks urls "$GO_URL"

routes
run tc-python "$TOOLCACHE" --out "$OUT" --python 3.12
expect "python: builds" passed
expect "python: checks the asset's digest" has urls "$PY_API"
expect "python: the container gets the checked tarball" has python-dl "$PY_TGZ"
expect "python: the container downloads nothing" lacks calls "$PY_URL"
expect "python: the drive records it" has downloads.sha256 "$PY_SHA  $PY_TGZ"

routes
reroute "$PY_URL" "$REMOTE/evil-python.tar.gz"
run tc-python-tampered "$TOOLCACHE" --out "$OUT" --python 3.12
expect "tampered python: fails" has err "$PY_TGZ: SHA-256 mismatch"
expect "tampered python: never reaches the container" lacks calls "docker run"

routes
reroute "$PY_API" "$REMOTE/python-release-old.json"
run tc-python-nodigest "$TOOLCACHE" --out "$OUT" --python 3.12
expect "python without a digest: fails and says how to pin" has err "GitHub records no SHA-256 for $PY_TGZ; pin it with --checksums"
expect "python without a digest: never reaches the container" lacks calls "docker run"

printf '%s  %s\n' "$PY_SHA" "$PY_TGZ" >"$TMP/pins"
run tc-python-pinned "$TOOLCACHE" --out "$OUT" --python 3.12 --checksums "$TMP/pins"
expect "python pinned: builds" passed
expect "python pinned: asks no API" lacks urls "$PY_API"

routes
reroute "$PY_MANIFEST" "$REMOTE/python-outside.json"
run tc-python-outside "$TOOLCACHE" --out "$OUT" --python 3.12
expect "python from elsewhere: refused" has err "the manifest points outside"
expect "python from elsewhere: downloads nothing" lacks urls example.com

routes
run tc-codeql "$TOOLCACHE" --out "$OUT" --codeql "$CQ"
expect "codeql: builds" passed
expect "codeql: the bundle is on the drive" has drive "./CodeQL/$CQ/x64/codeql/codeql"
expect "codeql: the drive records it" has downloads.sha256 "$CQ_SHA  codeql-bundle-v$CQ-linux64.tar.gz"

reroute "$CQ_URL" "$REMOTE/evil-codeql.tar.gz"
run tc-codeql-tampered "$TOOLCACHE" --out "$OUT" --codeql "$CQ"
expect "tampered codeql: fails" has err "codeql-bundle-v$CQ-linux64.tar.gz: SHA-256 mismatch"
expect "tampered codeql: no drive" lacks calls mkfs.ext4

routes
printf '%s *%s\n' "$(printf '%064d' 4)" "$NODE_TGZ" >"$TMP/pins-wrong"
run tc-pinned-wrong "$TOOLCACHE" --out "$OUT" --node "$NV" --checksums "$TMP/pins-wrong"
expect "a wrong pin wins over upstream: fails" has err "$NODE_TGZ: SHA-256 mismatch: expected $(printf '%064d' 4)"
expect "a wrong pin: upstream not asked" lacks urls SHASUMS256.txt

run tc-pins-missing "$TOOLCACHE" --out "$OUT" --node "$NV" --checksums "$TMP/no-such-file"
expect "a missing --checksums file: refused" has err "no such file"

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
