#!/usr/bin/env bash
#
# build-ubuntu-rootfs.sh — build a kitchen-sink, ubuntu-latest-parity firerunner
# rootfs (ext4) via Docker (OCI -> ext4), the on-prem equivalent of the
# Ubicloud approach (actions/runner-images provisioning, minus the Azure Packer
# flow). Produces a BOOTABLE microVM image: systemd init + actions/runner +
# firerunner MMDS-JIT boot service.
#
# Runs on a Linux KVM host with Docker (tested on the Arch/Omarchy 'starship').
#
# Usage:
#   sudo ./build-ubuntu-rootfs.sh --out /var/tmp/fr-golden/ubuntu-rootfs.ext4 \
#     --toolset base            # minimal = thin ubuntu-parity base: no docker,
#                               #        no baked language toolchains -- pair it
#                               #        with a --toolcache drive (build-toolcache.sh)
#                               #        for node/go/python/etc via setup-* actions
#                               # base = minimal + docker.io + curated kitchen-sink
#                               #        language toolchains (ubuntu-latest-ish)
#                               # full = base + actions/runner-images toolcache
#                               #        + curated docker-safe installer subset
#                               #        (Azure/GUI/snap/services excluded)
#     [--cache-redirect]        # patch the runner agent so firerunner can point
#                               #        actions/cache at a self-hosted cache-
#                               #        server; pair with firerunner --cache-port
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ASSETS="$HERE/assets"

OUT=""
TOOLSET="base"
RUNNER_VERSION=""
NODE_VERSION="22.11.0"
UBUNTU_TAG="24.04"
RI_REF="ubuntu24/20260816.277"   # actions/runner-images pinned ref for --toolset full
DNS_SERVERS="1.1.1.1 8.8.8.8"
SIZE_MB=""              # empty => sized from rootfs du + max(margin, free floor)
MARGIN_PCT=35
FREE_MB=12288           # absolute writable free space to guarantee for job
                        # scratch (CodeQL databases, large builds, _work/_temp).
                        # The image is sparse + reflink-cloned, so a big virtual
                        # size is near-free on disk; a lean rootfs would otherwise
                        # leave only a percentage of its own small size free,
                        # which is far too little for real CI jobs.
IMAGE_TAG="firerunner-ubuntu-rootfs:latest"
CACHE_REDIRECT=0        # when 1, patch the runner agent so it cannot override
                        # ACTIONS_RESULTS_URL, letting firerunner point
                        # actions/cache at a self-hosted cache-server (see the
                        # dependency-cache section in README.md).

die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)            OUT="$2"; shift 2 ;;
    --toolset)        TOOLSET="$2"; shift 2 ;;
    --runner-version) RUNNER_VERSION="$2"; shift 2 ;;
    --node-version)   NODE_VERSION="$2"; shift 2 ;;
    --ubuntu-tag)     UBUNTU_TAG="$2"; shift 2 ;;
    --runner-images-ref) RI_REF="$2"; shift 2 ;;
    --dns-servers)    DNS_SERVERS="${2//,/ }"; shift 2 ;;
    --size-mb)        SIZE_MB="$2"; shift 2 ;;
    --free-mb)        FREE_MB="$2"; shift 2 ;;
    --cache-redirect) CACHE_REDIRECT=1; shift ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux (KVM host)"
[[ $EUID -eq 0 ]] || die "must run as root"
[[ -n "$OUT" ]] || die "--out is required"
case "$TOOLSET" in minimal|base|full) ;; *) die "--toolset must be minimal, base or full" ;; esac
for t in docker mkfs.ext4 e2label; do command -v "$t" >/dev/null || die "missing tool: $t"; done
docker info >/dev/null 2>&1 || die "docker daemon not available"

if [[ -z "$RUNNER_VERSION" ]]; then
  RUNNER_VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest \
    | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$RUNNER_VERSION" ]] || die "could not resolve latest actions/runner version"
fi

echo ">> toolset=$TOOLSET ubuntu=$UBUNTU_TAG runner=$RUNNER_VERSION out=$OUT"
[[ "$TOOLSET" == "full" ]] && echo ">> runner-images ref=$RI_REF"
[[ "$CACHE_REDIRECT" == "1" ]] && echo ">> cache-redirect: runner will be patched for self-hosted actions/cache"

WORK="$(mktemp -d)"
CTX="$WORK/ctx"
ROOT="$WORK/root"
mkdir -p "$CTX" "$ROOT"
cleanup() { rm -rf "$WORK"; docker rm -f fr-rootfs-export >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Boot assets go into the build context.
cp "$ASSETS/firerunner-run.sh" "$CTX/firerunner-run.sh"
cp "$ASSETS/firerunner-runner.service" "$CTX/firerunner-runner.service"
cp "$ASSETS/docker-shim.sh" "$CTX/docker-shim.sh"

# resolv.conf matching the egress allowlist's resolvers.
{ for ns in $DNS_SERVERS; do echo "nameserver $ns"; done; } > "$CTX/resolv.conf"

# ---- Stage 2: vendor actions/runner-images (only for --toolset full) --------
# We stage the upstream build scripts + the official toolset.json into the build
# context at a pinned ref, then run a curated, docker-safe subset inside the
# image build. This gives real ubuntu-latest parity for the toolcache
# (setup-python/node/go/etc.) without the Azure Packer / GUI / snap machinery.
if [[ "$TOOLSET" == "full" ]]; then
  echo ">> vendoring actions/runner-images @ $RI_REF"
  RI_TGZ="$WORK/runner-images.tar.gz"
  curl -fsSL -o "$RI_TGZ" \
    "https://github.com/actions/runner-images/archive/refs/tags/${RI_REF}.tar.gz" \
    || die "could not download runner-images @ $RI_REF"
  RI_SRC="$WORK/ri-src"
  mkdir -p "$RI_SRC"
  tar -xzf "$RI_TGZ" -C "$RI_SRC" --strip-components=1
  SCRIPTS_DIR="$RI_SRC/images/ubuntu/scripts"
  TOOLSET_JSON="$RI_SRC/images/ubuntu/toolsets/toolset-2404.json"
  [[ -d "$SCRIPTS_DIR" && -f "$TOOLSET_JSON" ]] || die "unexpected runner-images layout for $RI_REF"

  # Recreate the runner-images on-VM layout the scripts assume:
  #   /imagegeneration/{helpers,tests,installers}, toolset.json in installers.
  IG="$CTX/imagegeneration"
  mkdir -p "$IG/helpers" "$IG/tests" "$IG/installers"
  cp -a "$SCRIPTS_DIR/helpers/." "$IG/helpers/"
  cp -a "$SCRIPTS_DIR/tests/."   "$IG/tests/"
  cp -a "$SCRIPTS_DIR/build/."   "$IG/installers/"
  cp "$TOOLSET_JSON" "$IG/installers/toolset.json"

  # Neutralise the Pester self-tests upstream scripts invoke after install; they
  # need the full test harness/runtime we don't reproduce in a docker build.
  # A later definition wins in bash, so append a no-op override to the helper
  # every build script sources.
  printf '\n# firerunner: skip upstream Pester self-tests in docker build\ninvoke_tests() { echo "firerunner: skip tests: $*"; }\n' \
    >> "$IG/helpers/install.sh"

  # Same for the PowerShell installers: strip the trailing Invoke-PesterTests
  # calls (they need the full Pester harness we do not reproduce in a build).
  for ps1 in "$IG/installers"/*.ps1; do
    [[ -f "$ps1" ]] && sed -i.bak '/Invoke-PesterTests/d' "$ps1" && rm -f "$ps1.bak"
  done

  # Curated driver: runs a docker-safe subset in dependency order. Azure, GUI/
  # browser/selenium, android-sdk, snap- and service-dependent installers are
  # intentionally excluded.
  cp "$ASSETS/ri-run.sh" "$CTX/ri-run.sh" 2>/dev/null || cat > "$CTX/ri-run.sh" <<'RIRUN'
#!/bin/bash
# Curated runner-images installer subset for a docker->ext4 microVM golden.
# Excluded on purpose: Azure (azcopy/azure-cli/az-devops/bicep/az modules),
# GUI/browser/selenium/android-sdk, snap- and service-dependent installers
# (mysql/postgresql/mssql), homebrew. Extend ALLOWLIST as needs grow.
set -uo pipefail
export HELPER_SCRIPTS=/imagegeneration/helpers
export INSTALLER_SCRIPT_FOLDER=/imagegeneration/installers
export DEBIAN_FRONTEND=noninteractive
export SUDO_USER=root
export AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
mkdir -p "$AGENT_TOOLSDIRECTORY"
cd "$INSTALLER_SCRIPT_FOLDER"

ALLOWLIST=(
  install-apt-common.sh
  install-github-cli.sh
  install-git-lfs.sh
  install-yq.sh
  install-zstd.sh
)
for s in "${ALLOWLIST[@]}"; do
  echo "==> $s"
  bash "$INSTALLER_SCRIPT_FOLDER/$s" || { echo "FAILED: $s"; exit 1; }
done

# Toolcache parity (Python/Node/Go/PyPy/CodeQL at official versions) via the
# upstream PowerShell installers -> /opt/hostedtoolcache.
echo "==> Install-Toolset.ps1"
pwsh -f "$INSTALLER_SCRIPT_FOLDER/Install-Toolset.ps1" || { echo "FAILED: Install-Toolset.ps1"; exit 1; }
echo "==> Configure-Toolset.ps1"
pwsh -f "$INSTALLER_SCRIPT_FOLDER/Configure-Toolset.ps1" || { echo "FAILED: Configure-Toolset.ps1"; exit 1; }
echo "==> runner-images toolset subset complete"
RIRUN
  chmod +x "$CTX/ri-run.sh"
fi

# ---- Dockerfile ------------------------------------------------------------
# Language toolchains baked into the rootfs. `minimal` externalizes languages to
# a --toolcache drive (build-toolcache.sh) and drops docker; base/full bake the
# ubuntu-latest-ish kitchen sink (incl. docker.io). build-essential + system
# python3 stay in every toolset (node-gyp/native modules + ubuntu-latest parity).
TOOLCHAIN_PKGS="build-essential pkg-config make cmake python3 python3-pip python3-venv python-is-python3 libssl-dev libffi-dev zlib1g-dev libbz2-dev libreadline-dev libsqlite3-dev"
if [[ "$TOOLSET" != "minimal" ]]; then
  TOOLCHAIN_PKGS="$TOOLCHAIN_PKGS default-jdk ruby-full golang-go docker.io"
fi

cat > "$CTX/Dockerfile" <<DOCKERFILE
FROM ubuntu:${UBUNTU_TAG}
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

# init + base runtime the microVM and the runner need.
RUN apt-get update && apt-get install -y --no-install-recommends \\
      systemd systemd-sysv ca-certificates curl wget git tar xz-utils gzip zstd unzip zip \\
      jq sudo iproute2 iputils-ping openssh-client rsync gnupg lsb-release \\
      software-properties-common locales tzdata \\
    && rm -rf /var/lib/apt/lists/*

# Language toolchains (see TOOLCHAIN_PKGS above): kitchen sink + docker for
# base/full, thin build-essential + system python3 only for minimal.
RUN apt-get update && apt-get install -y --no-install-recommends \\
      ${TOOLCHAIN_PKGS} \\
    && rm -rf /var/lib/apt/lists/*

# Node.js LTS from the official static tarball (no nodesource) into /usr/local.
RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz" \\
      | tar -xz -C /usr/local --strip-components=1 && node --version

# GitHub Actions runner agent.
RUN install -d /opt/runner \\
    && curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \\
      | tar -xz -C /opt/runner \\
    && /opt/runner/bin/installdependencies.sh \\
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE

# ---- Optional: cache-redirect patch ----------------------------------------
# firerunner runs the REAL GitHub runner, which overwrites ACTIONS_RESULTS_URL
# with the value from GitHub's per-job message — so simply exporting our own URL
# in the boot script is not enough to redirect actions/cache. We rename the
# ACTIONS_RESULTS_URL string inside the runner worker to the (dead) name
# ACTIONS_RESULTS_ORL: the runner then writes GitHub's URL into that dead name,
# while actions/cache (a separate Node process) still reads the real
# ACTIONS_RESULTS_URL that firerunner-run.sh exported. The rename is same-length,
# so binary offsets are preserved and the runner keeps working. This golden MUST
# be paired with a firerunner --cache-port/--cache-url deployment: only then does
# firerunner-run.sh export ACTIONS_RESULTS_URL. On its own the runner no longer
# sets it either (it now writes the dead name), so actions/cache has no endpoint
# and caching is disabled — the job still runs, just without any cache.
#
# The string lives in the .NET assembly's user-string heap as UTF-16LE (a null
# byte after each character), so we patch that encoding (with an ASCII fallback
# for future layouts). perl is used because it handles NUL bytes cleanly; it is
# an Essential package on Ubuntu, so it is always present in the build image.
if [[ "$CACHE_REDIRECT" == "1" ]]; then
cat >> "$CTX/Dockerfile" <<'DOCKERFILE'

RUN set -eu; \
    dll="$(find /opt/runner -name Runner.Worker.dll -print -quit)"; \
    [ -n "$dll" ] || { echo "cache-redirect: Runner.Worker.dll not found" >&2; exit 1; }; \
    perl -0777 -ne 'my $w=join("",map{$_."\x00"}split//,"ACTIONS_RESULTS_URL"); exit((index($_,$w)>=0||index($_,"ACTIONS_RESULTS_URL")>=0)?0:1)' "$dll" \
      || { echo "cache-redirect: marker ACTIONS_RESULTS_URL absent (runner layout changed?)" >&2; exit 1; }; \
    perl -0777 -i -pe 'my $a=join("",map{$_."\x00"}split//,"ACTIONS_RESULTS_URL"); my $b=join("",map{$_."\x00"}split//,"ACTIONS_RESULTS_ORL"); s/\Q$a\E/$b/g; s/ACTIONS_RESULTS_URL/ACTIONS_RESULTS_ORL/g' "$dll"; \
    perl -0777 -ne 'my $w=join("",map{$_."\x00"}split//,"ACTIONS_RESULTS_ORL"); exit((index($_,$w)>=0||index($_,"ACTIONS_RESULTS_ORL")>=0)?0:1)' "$dll" \
      || { echo "cache-redirect: patch did not take" >&2; exit 1; }; \
    echo "firerunner: cache-redirect patch applied to $dll"
DOCKERFILE
fi

# ---- Stage 2 full: layer runner-images toolcache + curated installers ------
if [[ "$TOOLSET" == "full" ]]; then
cat >> "$CTX/Dockerfile" <<DOCKERFILE

# PowerShell (drives the upstream toolcache installers).
RUN curl -fsSL "https://packages.microsoft.com/config/ubuntu/${UBUNTU_TAG}/packages-microsoft-prod.deb" -o /tmp/pmc.deb \\
    && dpkg -i /tmp/pmc.deb && rm -f /tmp/pmc.deb \\
    && apt-get update && apt-get install -y --no-install-recommends powershell \\
    && rm -rf /var/lib/apt/lists/*

# Vendored actions/runner-images build scripts + official toolset.json, run
# through the curated driver (see ri-run.sh for the allowlist + exclusions).
COPY imagegeneration /imagegeneration
COPY ri-run.sh /usr/local/bin/ri-run.sh
RUN chmod +x /usr/local/bin/ri-run.sh /imagegeneration/installers/*.sh /imagegeneration/helpers/*.sh 2>/dev/null; \\
    /usr/local/bin/ri-run.sh
DOCKERFILE
fi

# ---- Dockerfile finalize: boot service + init -----------------------------
# Container jobs (a workflow job with `container:` and/or `services:`) run
# `docker version` at job PICKUP -- before any step -- to create the job and
# service containers. So dockerd must be READY the moment the runner accepts a
# job. Socket-activation alone (docker.socket, dockerd off) races that: under a
# concurrent matrix some legs accept a job before docker.socket is up and fail
# setup with "dial unix /var/run/docker.sock: no such file". For toolsets that
# ship docker (base/full) we therefore start dockerd at boot and gate the runner
# behind it with Requires= + After= on docker.service (Type=notify): the runner
# starts only once dockerd is active, and not at all if Docker fails -- instead
# of accepting a container job with no daemon. The minimal toolset has no docker,
# so it keeps docker.service disabled.
if [[ "$TOOLSET" == "minimal" ]]; then
  DOCKER_BOOT='for u in docker.service containerd.service postgresql.service mysql.service apache2.service nginx.service; do systemctl disable "$u" 2>/dev/null || true; done'
else
  DOCKER_BOOT='systemctl enable docker.service && mkdir -p /etc/systemd/system/firerunner-runner.service.d && printf "[Unit]\nAfter=docker.service\nRequires=docker.service\n" > /etc/systemd/system/firerunner-runner.service.d/10-docker.conf && for u in postgresql.service mysql.service apache2.service nginx.service; do systemctl disable "$u" 2>/dev/null || true; done'
fi

# Docker toolsets also get the docker shim ahead of /usr/bin/docker on PATH so
# a cache-redirect golden's ACTIONS_RESULTS_URL reaches steps inside `container:`
# jobs (see assets/docker-shim.sh). It is inert unless that variable is set.
DOCKER_SHIM=""
if [[ "$TOOLSET" != "minimal" ]]; then
  DOCKER_SHIM='COPY docker-shim.sh /usr/local/bin/docker
RUN chmod 0755 /usr/local/bin/docker'
fi

cat >> "$CTX/Dockerfile" <<DOCKERFILE

# firerunner MMDS-JIT boot service (fetch jitconfig -> run one job -> reboot -f).
COPY firerunner-run.sh /usr/local/bin/firerunner-run.sh
COPY firerunner-runner.service /etc/systemd/system/firerunner-runner.service
COPY resolv.conf /etc/resolv.conf
$DOCKER_SHIM

# Ephemeral microVM boot policy: the runner always starts at boot; DBs and web
# servers stay installed but disabled (a throwaway VM shouldn't spend minutes
# bringing up daemons it may never use). Docker is per-toolset: base/full start
# dockerd at boot and gate the runner behind it (Requires=+After=, see
# DOCKER_BOOT above) so a container job never races an un-started daemon; minimal
# has no docker and keeps docker.service disabled. Snap, apt timers,
# networkd-wait-online and other noise are masked outright.
RUN chmod 0755 /usr/local/bin/firerunner-run.sh \\
    && ln -sf /lib/systemd/systemd /sbin/init \\
    && systemctl enable firerunner-runner.service \\
    && ${DOCKER_BOOT} \\
    && for u in snapd.service snapd.socket snapd.seeded.service apt-daily.timer apt-daily-upgrade.timer motd-news.timer motd-news.service e2scrub_all.timer e2scrub_reap.service dpkg-db-backup.timer man-db.timer unattended-upgrades.service systemd-networkd-wait-online.service serial-getty@ttyS0.service; do systemctl mask "\$u" 2>/dev/null || true; done
DOCKERFILE

echo ">> docker build ($TOOLSET)"
docker build -t "$IMAGE_TAG" "$CTX"

# ---- OCI -> rootfs dir -> ext4 --------------------------------------------
echo ">> exporting container filesystem"
docker create --name fr-rootfs-export "$IMAGE_TAG" true >/dev/null
docker export fr-rootfs-export | tar -C "$ROOT" -xf -
docker rm -f fr-rootfs-export >/dev/null

# systemd inside a VM needs these dirs; docker export omits some.
mkdir -p "$ROOT/proc" "$ROOT/sys" "$ROOT/dev" "$ROOT/run" "$ROOT/tmp"
chmod 1777 "$ROOT/tmp"

# docker bind-mounts /etc/resolv.conf, /etc/hosts and /etc/hostname at runtime,
# so `docker export` captures them as EMPTY files and shadows anything the
# Dockerfile COPY'd in. An empty resolv.conf means the guest has no nameserver,
# and the runner's connect to *.actions.githubusercontent.com fails with
# EAI_AGAIN ("Resource temporarily unavailable"). Write them into the extracted
# rootfs directly, after export, so the microVM boots with working DNS.
{ for ns in $DNS_SERVERS; do echo "nameserver $ns"; done; } > "$ROOT/etc/resolv.conf"
cat > "$ROOT/etc/hosts" <<'HOSTS'
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
HOSTS
echo firerunner > "$ROOT/etc/hostname"

if [[ -z "$SIZE_MB" ]]; then
  used_mb="$(du -sm "$ROOT" | awk '{print $1}')"
  margin_mb=$(( used_mb * MARGIN_PCT / 100 ))
  free_mb=$(( margin_mb > FREE_MB ? margin_mb : FREE_MB ))
  SIZE_MB=$(( used_mb + free_mb + 512 ))
fi
echo ">> rootfs sized ${SIZE_MB}MB (used ${used_mb:-?}MB + free scratch)"

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
mkfs.ext4 -q -F -d "$ROOT" "$OUT" "${SIZE_MB}M"
# ext4 volume labels are capped at 16 bytes, so keep this short (avoid silent
# truncation). It is cosmetic — firerunner boots the rootfs via is_root_device /
# root=/dev/vda, not by label — so a stable role name is enough for every tier.
e2label "$OUT" firerunner-root || true

sync
echo ">> rootfs image written: $OUT ($(du -h "$OUT" | awk '{print $1}'))"
echo ">> next: boot-test in firecracker, then deploy as a tier image"
