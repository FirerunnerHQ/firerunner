#!/usr/bin/env bash
# Tests for layer-ec2-ami.sh with fake aws, curl, gh and system commands: no AWS
# account, no root, and no real disk.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
export STATE="$TMP/state"

cat >"$TMP/bin/aws" <<'FAKE'
#!/usr/bin/env bash
echo "aws $*" >>"$STATE/calls"
case "$*" in
*"ssm get-parameter"*) echo ami-0param ;;
*"ec2 describe-images"*"BlockDeviceMappings"*) printf 'snap-0base\tuefi-preferred\tTrue\n' ;;
*"ec2 describe-images"*) cat "$STATE/image-state" 2>/dev/null || echo available ;;
*"ec2 create-volume"*) echo vol-0new ;;
*"ec2 describe-volumes"*"Attachments"*) echo attached ;;
*"ec2 describe-volumes"*) echo available ;;
*"ec2 create-snapshot"*) echo snap-0new ;;
*"ec2 describe-snapshots"*) echo completed ;;
*"ec2 register-image"*) echo ami-0new ;;
*) ;;
esac
FAKE
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
for a; do last="$a"; done
case "$last" in
*latest/api/token) echo token ;;
*instance-id) echo i-0self ;;
*availability-zone) echo us-east-1a ;;
*SHA256SUMS)
	echo "$last" >>"$STATE/downloads"
	for a; do [ "$prev" = -o ] && out="$a"; prev="$a"; done
	sum="$(sha256sum "$FAKE_BIN" 2>/dev/null || shasum -a 256 "$FAKE_BIN")"
	echo "${sum%% *}  firerunner_linux_amd64" >"$out"
	;;
*firerunner_linux_amd64)
	echo "$last" >>"$STATE/downloads"
	for a; do [ "$prev" = -o ] && out="$a"; prev="$a"; done
	cp "$FAKE_BIN" "$out"
	# A release replaced after its check: the next download is another binary,
	# with a SHA256SUMS of its own to match.
	[ ! -f "$STATE/replaced" ] || printf '#!/bin/sh\nexit 0 # replaced\n' >"$FAKE_BIN"
	;;
*) exit 22 ;;
esac
FAKE
# Fake gh: every binary carries its attestation unless $STATE/unattested.
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
echo "gh $*" >>"$STATE/gh"
[ "$1 $2" = "attestation verify" ] || exit 1
[ ! -f "$STATE/unattested" ] || { echo "Error: no matching attestations found" >&2; exit 1; }
FAKE
export FAKE_BIN="$TMP/fake-firerunner"
cat >"$TMP/bin/lsblk" <<'FAKE'
#!/bin/sh
echo "/dev/fake14"
echo "/dev/fake1 cloudimg-rootfs"
FAKE
cat >"$TMP/bin/mount" <<'FAKE'
#!/bin/sh
echo "mount $*" >>"$STATE/calls"
FAKE
cat >"$TMP/bin/umount" <<'FAKE'
#!/bin/sh
echo "umount $*" >>"$STATE/calls"
FAKE
cat >"$TMP/bin/chroot" <<'FAKE'
#!/bin/sh
echo "chroot $*" >>"$STATE/calls"
cp "$1/$3" "$STATE/assets"
[ ! -e "$STATE/chroot-fails" ] || exit 1
FAKE
chmod +x "$TMP/bin"/*
if command -v sha256sum >/dev/null; then :; else
	printf '#!/bin/sh\nexec shasum -a 256 "$@"\n' >"$TMP/bin/sha256sum"
	chmod +x "$TMP/bin/sha256sum"
fi

fails=0
expect() { # DESC CMD...
	local desc="$1"
	shift
	if "$@"; then echo "ok   $desc"; else
		echo "FAIL $desc"
		fails=$((fails + 1))
	fi
}
called() { grep -qF -- "$1" "$STATE/calls"; }
not_called() { ! called "$1"; }
not_grep() { ! grep -q "$@"; }
ordered() {
	local a b
	a="$(grep -nF -- "$1" "$STATE/calls" | head -1 | cut -d: -f1)"
	b="$(grep -nF -- "$2" "$STATE/calls" | tail -1 | cut -d: -f1)"
	[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}
fresh() {
	rm -rf "$STATE" "$TMP/root"
	mkdir -p "$STATE" "$TMP/root/tmp" "$TMP/root/usr/local/bin"
	: >"$STATE/calls"
	printf '#!/bin/sh\nexit 0\n' >"$FAKE_BIN"
	chmod +x "$FAKE_BIN"
}
run() {
	rc=0
	PATH="$TMP/bin:$PATH" FIRERUNNER_LAYER_ROOT="$TMP/root" FIRERUNNER_LAYER_DEVICE=/dev/fake FIRERUNNER_LAYER_PAUSE=0 FIRERUNNER_LAYER_TRIES=3 \
		"$HERE/layer-ec2-ami.sh" "$@" >"$STATE/out" 2>"$STATE/err" || rc=$?
}

fresh
run --base /p/ami --ssm-param /p/new --firerunner-version v1.2.3
expect "exits 0" [ "$rc" = 0 ]
expect "reads a parameter's image" called "ec2 describe-images --image-ids ami-0param"
expect "starts from the base's snapshot, in this zone" called "ec2 create-volume --snapshot-id snap-0base --availability-zone us-east-1a"
expect "attaches to this instance" called "ec2 attach-volume --volume-id vol-0new --instance-id i-0self"
expect "mounts the root partition" called "mount /dev/fake1 $TMP/root"
expect "installs the release's binary" test -x "$TMP/root/usr/local/bin/firerunner"
expect "the tune script is in the assets" grep -q 'firerunner-ec2-tune' "$STATE/assets"
expect "the runner unit is in the assets" grep -q 'firerunner-ec2-runner.service' "$STATE/assets"
expect "unmounts before the snapshot" ordered "umount $TMP/root" "ec2 create-snapshot"
expect "detaches before the snapshot" ordered "ec2 detach-volume" "ec2 create-snapshot"
expect "keeps the base's boot mode" called "--boot-mode uefi-preferred"
expect "keeps ENA" called "--ena-support"
expect "publishes after the image is ready" ordered "ec2 register-image" "ssm put-parameter --name /p/new"
expect "deletes the volume" called "ec2 delete-volume --volume-id vol-0new"
expect "prints the image" grep -q '"ami":"ami-0new"' "$STATE/out"

expect "checks the release's attestation" grep -q 'gh attestation verify .*/firerunner_linux_amd64 --repo FirerunnerHQ/firerunner --signer-workflow FirerunnerHQ/firerunner/.github/workflows/release.yml --source-ref refs/tags/v1.2.3 --deny-self-hosted-runners$' "$STATE/gh"
expect "installs only what it checked, not by the release's SHA256SUMS" not_grep SHA256SUMS "$STATE/downloads"

# A binary without the attestation: refused before anything is made.
fresh
: >"$STATE/unattested"
run --base ami-0x --ssm-param /p/new --firerunner-version v1.2.3
expect "unattested: fails" [ "$rc" != 0 ]
expect "unattested: says why, and how to skip" grep -q 'FIRERUNNER_SKIP_ATTESTATION=1 skips' "$STATE/err"
expect "unattested: no volume" not_called "ec2 create-volume"
expect "unattested: nothing installed" test ! -e "$TMP/root/usr/local/bin/firerunner"

# The release replaced after its check, SHA256SUMS and all: the download is
# held to the SHA-256 that was checked.
fresh
: >"$STATE/replaced"
run --base ami-0x --ssm-param /p/new --firerunner-version v1.2.3
expect "replaced after the check: fails" [ "$rc" != 0 ]
expect "replaced after the check: nothing installed" test ! -e "$TMP/root/usr/local/bin/firerunner"
expect "replaced after the check: nothing published" not_called "ssm put-parameter"

fresh
FIRERUNNER_SKIP_ATTESTATION=1 run --base ami-0x --ssm-param /p/new --firerunner-version v1.2.3
expect "skip: exits 0" [ "$rc" = 0 ]
expect "skip: says so" grep -q 'FIRERUNNER_SKIP_ATTESTATION=1: not checking' "$STATE/err"
expect "skip: no gh" test ! -f "$STATE/gh"
expect "skip: the release's SHA256SUMS instead" grep -q SHA256SUMS "$STATE/downloads"

fresh
run --base ami-0x --ssm-param /p/new --firerunner-version latest
expect "only a release tag" [ "$rc" != 0 ]
expect "a bad tag touches nothing" not_called "ec2 create-volume"

fresh
: >"$STATE/chroot-fails"
run --base ami-0x --ssm-param /p/new --firerunner-version v1.2.3
expect "a failed install fails" [ "$rc" != 0 ]
expect "a failed install publishes nothing" not_called "ssm put-parameter"
expect "a failed install takes no snapshot" not_called "ec2 create-snapshot"
expect "a failed install unmounts" called "umount $TMP/root"
expect "a failed install deletes the volume" called "ec2 delete-volume --volume-id vol-0new"

fresh
echo failed >"$STATE/image-state"
run --base ami-0x --ssm-param /p/new --firerunner-version v1.2.3
expect "an image that never comes fails" [ "$rc" != 0 ]
expect "and is not published" not_called "ssm put-parameter"

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
