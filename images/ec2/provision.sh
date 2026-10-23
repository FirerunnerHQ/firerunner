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
	docker.io zstd curl jq git ca-certificates

# Docker with the containerd image store, as on GitHub-hosted runners.
install -d /etc/docker
cat >/etc/docker/daemon.json <<'JSON'
{
  "features": { "containerd-snapshotter": true },
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
