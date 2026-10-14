#!/usr/bin/env bash
#
# build-smoke-rootfs.sh — build a tiny self-destructing rootfs for the firerunner
# boot/cache e2e. The rootfs contains a static Go PID1 (smoke_init.go) at
# /sbin/init that performs the requested cache checks and reboots, validating
# real guest HTTP and self-destruct without a full golden image or GitHub App.
#
# Linux + root only (needs loop mount + mknod). Output defaults to
# /var/tmp/fr-smoke/rootfs.ext4; pair it with a guest kernel via FR_KERNEL.
#
#   sudo ./test/e2e/build-smoke-rootfs.sh
#   FR_KERNEL=/path/vmlinux FR_GOLDEN=/var/tmp/fr-smoke/rootfs.ext4 make e2e
#
set -euo pipefail

OUT="${1:-/var/tmp/fr-smoke/rootfs.ext4}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "error: $*" >&2; exit 1; }

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux"
[[ $EUID -eq 0 ]] || die "must run as root (loop mount + mknod)"
command -v go >/dev/null || die "go toolchain required to build the init"

mkdir -p "$(dirname "$OUT")"
work="$(mktemp -d)"
trap 'mountpoint -q "$work/mnt" && umount "$work/mnt"; rm -rf "$work"' EXIT

echo ">> building static PID1 init"
CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o "$work/init" "$HERE/smoke_init.go"

echo ">> creating ext4 image at $OUT"
rm -f "$OUT"
truncate -s 64M "$OUT"
mkfs.ext4 -q -F "$OUT"

mkdir -p "$work/mnt"
mount -o loop "$OUT" "$work/mnt"
mkdir -p "$work/mnt/sbin" "$work/mnt/proc" "$work/mnt/dev"
install -m 0755 "$work/init" "$work/mnt/sbin/init"
mknod "$work/mnt/dev/console" c 5 1
mknod "$work/mnt/dev/null" c 1 3
sync
umount "$work/mnt"

echo ">> smoke rootfs ready: $OUT"
