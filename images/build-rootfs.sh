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
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ASSETS="$HERE/assets"

TIER="firerunner-4c8g"
RUNNER_VERSION=""            # empty => resolve latest from GitHub releases
NODE_VERSION="22.11.0"       # Node LTS baked into the firerunner-node tier
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
    --node-version)   NODE_VERSION="$2"; shift 2 ;;
    --out)            OUT="$2"; shift 2 ;;
    --size-mb)        SIZE_MB="$2"; shift 2 ;;
    --dns-servers)    DNS_SERVERS="${2//,/ }"; shift 2 ;;
    --suite)          SUITE="$2"; shift 2 ;;
    --mirror)         MIRROR="$2"; shift 2 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux (KVM host)"
[[ $EUID -eq 0 ]] || die "must run as root"
[[ -n "$OUT" ]] || die "--out is required"

case "$TIER" in
  firerunner-4c8g|firerunner-8c16g-docker|firerunner-node) ;;
  *) die "unknown tier: $TIER (want firerunner-4c8g, firerunner-8c16g-docker or firerunner-node)" ;;
esac

for t in mkfs.ext4 curl tar debootstrap; do
  command -v "$t" >/dev/null || die "missing required tool: $t"
done

# Resolve the latest runner release when not pinned. Kept within GitHub's
# 30-day support window by the scheduled rebuild workflow.
if [[ -z "$RUNNER_VERSION" ]]; then
  RUNNER_VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest \
    | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$RUNNER_VERSION" ]] || die "could not resolve latest actions/runner version"
fi

echo ">> tier=$TIER runner=$RUNNER_VERSION out=$OUT size=${SIZE_MB}MB suite=$SUITE"

WORK="$(mktemp -d)"
MNT="$WORK/mnt"
mkdir -p "$MNT"
cleanup() {
  for m in dev/pts dev proc sys; do
    mountpoint -q "$MNT/$m" && umount -l "$MNT/$m" || true
  done
  mountpoint -q "$MNT" && umount "$MNT" || true
  rm -rf "$WORK"
}
trap cleanup EXIT

in_chroot() { chroot "$MNT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive "$@"; }

# 1. Create + format the ext4 image and mount it.
mkdir -p "$(dirname "$OUT")"
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

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

# 3. Install the official actions/runner agent.
arch="x64"
runner_url="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${arch}-${RUNNER_VERSION}.tar.gz"
install -d "$MNT/opt/runner"
echo ">> downloading actions/runner v$RUNNER_VERSION"
curl -fsSL "$runner_url" | tar -xz -C "$MNT/opt/runner"
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
  node_url="https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz"
  curl -fsSL "$node_url" | tar -xz -C "$MNT/usr/local" --strip-components=1
  in_chroot /usr/local/bin/node --version
fi

# Trim apt caches to keep the image lean.
in_chroot apt-get clean || true
rm -rf "$MNT/var/lib/apt/lists/"* "$MNT/var/cache/apt/archives/"*.deb

sync
echo ">> golden image written: $OUT"
