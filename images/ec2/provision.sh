#!/bin/bash
# provision.sh — turns a stock Ubuntu 24.04 EC2 instance into a firerunner
# runner image, then powers off so build-ec2-ami.sh can snapshot it.
#
# It runs once, as root, from the build instance's user-data. build-ec2-ami.sh
# prepends the settings and an install_boot_unit function (the boot script and
# unit), so this file is never run by hand.
#
# What the image does at boot (firerunner-ec2-runner.service): fetch the JIT
# config over the VPC, run one job, power off. Everything here is about making
# that boot short and generic:
#   - no cloud-init or snapd, and no timers that wake up during a job;
#   - a netplan that matches any NIC (cloud-init's matches the builder's MAC);
#   - an empty hostname and machine-id, so each instance gets its own.
# And about making the job fast on a disk that lasts for one job:
#   - ext4 without the guarantees that protect a disk across a crash;
#   - firerunner-ec2-tune, which grows the disk and adds zram swap at boot.
#
# Settings (set by build-ec2-ami.sh):
#   RUNNER_VERSION  actions/runner version, e.g. 2.338.1
#   RUNNER_ARCH     x64 or arm64
set -euxo pipefail

: "${RUNNER_VERSION:?}" "${RUNNER_ARCH:?}"

mark() { echo "FIRERUNNER-BUILD $(date -u +%FT%TZ) $1" >/dev/console 2>/dev/null || true; }
# Any failure powers the instance off without the "finished" mark, so the
# build fails fast instead of waiting out its timeout. build-ec2-ami.sh tells
# a finished build from a failed one by the mark it finds on the console.
on_exit() {
	local rc=$?
	if [ "$rc" != 0 ]; then
		mark "FAILED rc=$rc"
		sync
		shutdown -h now
	fi
}
trap on_exit EXIT
mark start

export DEBIAN_FRONTEND=noninteractive
# First boot is when apt-daily and unattended-upgrades run and hold the dpkg
# lock: wait for it instead of failing at once.
apt_get() { apt-get -o DPkg::Lock::Timeout=600 "$@"; }
apt_get update
apt_get install -y --no-install-recommends \
	docker.io zstd curl jq git ca-certificates \
	nginx gcc libc6-dev cloud-guest-utils
# zram is in the kernel's extra modules on some Ubuntu kernels.
apt_get install -y --no-install-recommends "linux-modules-extra-$(uname -r)" || true

# Docker with the containerd image store, as on GitHub-hosted runners.
install -d /etc/docker
cat >/etc/docker/daemon.json <<'JSON'
{
  "features": { "containerd-snapshotter": true },
  "max-concurrent-downloads": 10,
  "log-driver": "json-file",
  "log-opts": { "max-size": "50m", "max-file": "2" }
}
JSON
systemctl enable docker.service containerd.service

# actions/runner. GitHub refuses runner versions more than ~30 days old, so the
# image must be rebuilt monthly (see images/README.md).
install -d /opt/runner
cd /opt/runner
curl -fsSL -o runner.tgz \
	"https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
tar xzf runner.tgz
rm runner.tgz
./bin/installdependencies.sh

# Boot unit and script: install_boot_unit is defined above, by build-ec2-ami.sh.
install_boot_unit

# The dependency cache proxy (#28), installed inactive: firerunner-ec2-run.sh
# runs firerunner-cache-setup only when the instance's tier caches. nginx
# starts then, not at boot.
install_cache_proxy
systemctl disable nginx.service
gcc -O2 -static -o /usr/local/libexec/nodewrap /tmp/nodewrap.c
rm -f /tmp/nodewrap.c
# Each node the runner bundles execs through nodewrap, which adds the cache
# CA when the bundle put it in externals (host and job containers alike).
for node in /opt/runner/externals/node*/bin/node; do
	mv "$node" "$node.real"
	install -m 0755 /usr/local/libexec/nodewrap "$node"
done
# Every wrapped node must still start, or every node action would fail.
# (node*_alpine is musl-built, for Alpine job containers: it cannot start on
# this glibc host, wrapped or not.)
for node in /opt/runner/externals/node*/bin/node; do
	case "$node" in */node*_alpine/*) continue ;; esac
	"$node" --version
done
apt_get purge -y --auto-remove gcc libc6-dev

# A netplan that matches any NIC. cloud-init's 50-cloud-init.yaml matches the
# builder's MAC address, which leaves every instance from this image without
# network once cloud-init no longer regenerates it.
rm -f /etc/netplan/50-cloud-init.yaml
cat >/etc/netplan/90-firerunner.yaml <<'NETPLAN'
network:
  version: 2
  ethernets:
    all-en:
      match:
        name: "en*"
      dhcp4: true
      dhcp6: false
NETPLAN
chmod 0600 /etc/netplan/90-firerunner.yaml
netplan generate

# Nothing should run at boot or during a job except the runner.
touch /etc/cloud/cloud-init.disabled
systemctl disable --now snapd.service snapd.socket snapd.seeded.service || true
for unit in apt-daily.timer apt-daily-upgrade.timer man-db.timer motd-news.timer \
	fstrim.timer e2scrub_all.timer unattended-upgrades.service \
	ModemManager.service multipathd.service multipathd.socket; do
	systemctl disable "$unit" 2>/dev/null || true
done

# Writes need not survive a crash: the disk is thrown away with the instance,
# and a pool instance stops cleanly. data= cannot change on a remount, so
# these go on the kernel command line.
cat >/etc/default/grub.d/90-firerunner.cfg <<'GRUB'
GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT rootflags=data=writeback,journal_async_commit,nobarrier quiet transparent_hugepage=always i8042.nokbd i8042.noaux i8042.nopnp"
GRUB
update-grub

# As in runs-on/runner-images-for-aws (MIT). dpkg's fsyncs take most of an
# install's time on EBS, and need not survive a crash here. xz, apt's default
# for its indexes, decompresses several times slower than gzip.
apt_get install -y --no-install-recommends eatmydata
cat >/usr/local/sbin/dpkg-eatmydata <<'DPKG'
#!/bin/sh
exec /usr/bin/eatmydata /usr/bin/dpkg "$@"
DPKG
chmod 0755 /usr/local/sbin/dpkg-eatmydata
echo 'Dir::Bin::dpkg "/usr/local/sbin/dpkg-eatmydata";' >/etc/apt/apt.conf.d/10dpkg-eatmydata
echo 'Acquire::CompressionTypes::Order:: "gz";' >/etc/apt/apt.conf.d/10apt-gzip-indexes

# Per-instance identity: an empty hostname is set from DHCP at boot, and an
# empty machine-id is regenerated, so instances do not share the builder's.
truncate -s 0 /etc/hostname
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id

apt-get clean
rm -rf /var/lib/apt/lists/* /var/log/journal/* /tmp/*
mark finished
sync
shutdown -h now
