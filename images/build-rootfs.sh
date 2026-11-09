#!/usr/bin/env bash
#
# build-rootfs.sh — build a firerunner golden rootfs (ext4) with the official
# actions/runner agent and the MMDS-JIT boot service pre-installed. Each microVM
# is reflink-cloned from the result, boots this image, runs one job, and
# self-destructs (see images/assets/firerunner-run.sh).
#
# Debian guest, built via debootstrap. Linux + root only; cannot run on macOS.
#
# Usage:
#   sudo ./build-rootfs.sh --tier firerunner-4c8g \
#     --runner-version 2.320.0 --out /var/lib/firerunner/golden-4c8g.ext4
#
# Every download is checked before it is used (see lib/verify.sh): the runner
# against the SHA-256 in its GitHub release notes, Node against the release's
# SHASUMS256.txt. --runner-sha256 and --node-sha256 pin them instead. The
# checksums used are printed and kept in /etc/firerunner/downloads.sha256.
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ASSETS="$HERE/assets"
# shellcheck source=lib/verify.sh disable=SC1091
. "$HERE/lib/verify.sh"

TIER="firerunner-4c8g"
RUNNER_VERSION=""            # empty => resolve latest from GitHub releases
RUNNER_SHA256=""             # empty => read from the release notes
NODE_VERSION="22.11.0"       # Node LTS baked into the firerunner-node tier
NODE_SHA256=""               # empty => read from the release's SHASUMS256.txt
OUT=""
SIZE_MB=8192
DNS_SERVERS="1.1.1.1 8.8.8.8"
SUITE="bookworm"
MIRROR="http://deb.debian.org/debian"

die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tier)           TIER="$2"; shift 2 ;;
    --runner-version) RUNNER_VERSION="$2"; shift 2 ;;
    --runner-sha256)  RUNNER_SHA256="$2"; shift 2 ;;
    --node-version)   NODE_VERSION="$2"; shift 2 ;;
    --node-sha256)    NODE_SHA256="$2"; shift 2 ;;
    --out)            OUT="$2"; shift 2 ;;
    --size-mb)        SIZE_MB="$2"; shift 2 ;;
    --dns-servers)    DNS_SERVERS="${2//,/ }"; shift 2 ;;
    --suite)          SUITE="$2"; shift 2 ;;
    --mirror)         MIRROR="$2"; shift 2 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux (KVM host)"
[[ "$(id -u)" -eq 0 ]] || die "must run as root"
[[ -n "$OUT" ]] || die "--out is required"

case "$TIER" in
  firerunner-4c8g|firerunner-8c16g-docker|firerunner-node) ;;
  *) die "unknown tier: $TIER (want firerunner-4c8g, firerunner-8c16g-docker or firerunner-node)" ;;
esac

for t in mkfs.ext4 curl tar debootstrap; do
  command -v "$t" >/dev/null || die "missing required tool: $t"
done

# Resolve the latest runner release when not pinned, once, with the SHA-256 its
# release notes give the tarball. Kept within GitHub's 30-day support window by
# the scheduled rebuild workflow.
if [[ -n "$RUNNER_SHA256" ]]; then
  [[ -n "$RUNNER_VERSION" ]] || die "--runner-sha256 needs --runner-version"
else
  release="$(runner_release "$RUNNER_VERSION" linux-x64)" || exit 1
  RUNNER_VERSION="${release% *}" RUNNER_SHA256="${release#* }"
fi
if [[ "$TIER" == "firerunner-node" && -z "$NODE_SHA256" ]]; then
  NODE_SHA256="$(node_sha256 "$NODE_VERSION" "node-v${NODE_VERSION}-linux-x64.tar.gz")" || exit 1
fi

echo ">> tier=$TIER runner=$RUNNER_VERSION out=$OUT size=${SIZE_MB}MB suite=$SUITE"
echo ">> actions/runner $RUNNER_VERSION linux-x64 sha256 $RUNNER_SHA256"

WORK="$(mktemp -d)"
MNT="$WORK/mnt"
mkdir -p "$MNT"
export VERIFIED_LOG="$WORK/downloads.sha256"
: >"$VERIFIED_LOG"
# The image is built beside --out and renamed over it only when complete, so
# a failed build leaves the previous image in place, never a half-built one.
PART="$OUT.partial"
cleanup() {
  for m in dev/pts dev proc sys; do
    mountpoint -q "$MNT/$m" && umount -l "$MNT/$m" || true
  done
  mountpoint -q "$MNT" && umount "$MNT" || true
  rm -rf "$WORK"
  rm -f "$PART"
}
trap cleanup EXIT

in_chroot() { chroot "$MNT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive "$@"; }

# 0. Download and check everything first: a bad download fails in seconds,
#    before the slow bootstrap.
arch="x64"
runner_tgz="actions-runner-linux-${arch}-${RUNNER_VERSION}.tar.gz"
runner_url="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${runner_tgz}"
echo ">> downloading actions/runner v$RUNNER_VERSION"
fetch_checked "$runner_url" "$WORK/$runner_tgz" "$RUNNER_SHA256" "$runner_tgz" || exit 1
if [[ "$TIER" == "firerunner-node" ]]; then
  node_tgz="node-v${NODE_VERSION}-linux-x64.tar.gz"
  node_url="https://nodejs.org/dist/v${NODE_VERSION}/${node_tgz}"
  fetch_checked "$node_url" "$WORK/$node_tgz" "$NODE_SHA256" "$node_tgz" || exit 1
fi

# 1. Create + format the ext4 image and mount it.
mkdir -p "$(dirname "$OUT")"
rm -f "$PART"
truncate -s "${SIZE_MB}M" "$PART"
mkfs.ext4 -q -F "$PART"
mount -o loop "$PART" "$MNT"

# 2. Bootstrap a minimal Debian base with an init and the tools the runner and
#    the boot service need.
BASE_PKGS="systemd-sysv,ca-certificates,curl,tar,zstd,git,iproute2,sudo,jq"
debootstrap --variant=minbase --include="$BASE_PKGS" "$SUITE" "$MNT" "$MIRROR"

# Bind mounts for chroot package operations.
mount --bind /dev "$MNT/dev"
mount --bind /dev/pts "$MNT/dev/pts"
mount -t proc proc "$MNT/proc"
mount -t sysfs sys "$MNT/sys"

# Bake a static resolv.conf matching the egress allowlist's --dns-servers. Done
# before any chroot apt/curl so build-time DNS works too (the same file serves
# the runtime microVM, whose egress allowlist only permits these resolvers).
rm -f "$MNT/etc/resolv.conf"
{
  for ns in $DNS_SERVERS; do echo "nameserver $ns"; done
} > "$MNT/etc/resolv.conf"

# 3. Install the official actions/runner agent, checked in step 0.
install -d "$MNT/opt/runner"
tar -xzf "$WORK/$runner_tgz" -C "$MNT/opt/runner"
rm -f "$WORK/$runner_tgz"
in_chroot /opt/runner/bin/installdependencies.sh

# 4. Install the MMDS-JIT boot service (fetch jitconfig -> run one job ->
#    reboot -f). See images/assets/.
install -m 0755 "$ASSETS/firerunner-run.sh" "$MNT/usr/local/bin/firerunner-run.sh"
install -m 0644 "$ASSETS/firerunner-runner.service" "$MNT/etc/systemd/system/firerunner-runner.service"
in_chroot systemctl enable firerunner-runner.service
# Disable getty/login prompts -- the microVM is headless and single-purpose.
in_chroot systemctl mask serial-getty@ttyS0.service || true

# 5. Bake a static resolv.conf matching the egress allowlist's --dns-servers.
rm -f "$MNT/etc/resolv.conf"
{
  for ns in $DNS_SERVERS; do echo "nameserver $ns"; done
} > "$MNT/etc/resolv.conf"

# 6. (docker tier) install and enable Docker for jobs using container:/services.
if [[ "$TIER" == "firerunner-8c16g-docker" ]]; then
  echo ">> installing Docker for the docker tier"
  in_chroot apt-get update
  in_chroot apt-get install -y --no-install-recommends docker.io
  in_chroot systemctl enable docker.service
fi

# 6b. (node tier) bake Node.js LTS so runs-on:firerunner-node jobs find node/npm
#     on PATH without a setup-node download (mirrors GitHub's hosted tool cache).
#     Installed from the official static tarball into /usr/local (no apt, no
#     nodesource) to keep the image minimal; --strip-components lands node in
#     /usr/local/bin.
if [[ "$TIER" == "firerunner-node" ]]; then
  echo ">> installing Node.js v$NODE_VERSION for the node tier"
  tar -xzf "$WORK/$node_tgz" -C "$MNT/usr/local" --strip-components=1
  rm -f "$WORK/$node_tgz"
  in_chroot /usr/local/bin/node --version
fi

# What the image was built from, as checked above.
install -d "$MNT/etc/firerunner"
install -m 0644 "$VERIFIED_LOG" "$MNT/etc/firerunner/downloads.sha256"

# Trim apt caches to keep the image lean.
in_chroot apt-get clean || true
rm -rf "$MNT/var/lib/apt/lists/"* "$MNT/var/cache/apt/archives/"*.deb

sync
for m in dev/pts dev proc sys; do umount "$MNT/$m"; done
umount "$MNT"
mv -f "$PART" "$OUT"
echo ">> golden image written: $OUT"
