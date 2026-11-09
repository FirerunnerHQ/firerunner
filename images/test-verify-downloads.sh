#!/usr/bin/env bash
# Tests that the image builders check what they download before they use it.
# Each builder runs end to end on any host: the commands that need root, Linux
# or the network are fakes. The fake curl serves files by URL from a routes
# table, so a test can hand out a tampered archive; tar and sha256sum are real.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
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
for t in mkfs.ext4 truncate mount umount e2label; do
	# shellcheck disable=SC2016 # the fake expands it
	printf '#!/bin/sh\necho "%s $*" >>"$STATE/calls"\n' "$t" >"$BIN/$t"
done
printf '#!/bin/sh\nexit 1\n' >"$BIN/mountpoint"
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

# routes: the honest upstream.
routes() {
	: >"$REMOTE/routes"
	serve https://api.github.com/repos/actions/runner/releases/latest "$REMOTE/release.json"
	serve "https://api.github.com/repos/actions/runner/releases/tags/v$RV" "$REMOTE/release.json"
	serve "$RUNNER_URL" "$REMOTE/$RUNNER_TGZ"
	serve "https://nodejs.org/dist/v$NV/SHASUMS256.txt" "$REMOTE/SHASUMS256.txt"
	serve "$NODE_URL" "$REMOTE/$NODE_TGZ"
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
run rootfs-node-unlisted "$ROOTFS" --out "$OUT" --tier firerunner-node
expect "node missing from SHASUMS256.txt: fails before building" failed
expect "node missing from SHASUMS256.txt: says so" has err "lists no $NODE_TGZ"
expect "node missing from SHASUMS256.txt: no image" lacks calls mkfs.ext4

run rootfs-node-pinned "$ROOTFS" --out "$OUT" --tier firerunner-node --node-sha256 "$NODE_SHA"
expect "node pinned: builds without SHASUMS256.txt" passed
expect "node pinned: installs node" exists node-installed

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
