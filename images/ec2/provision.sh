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
#   - firerunner-syn-reroll, which retries stalled SYNs on a new source port
#     for the first two minutes;
#   - firerunner-ec2-tune, which grows the disk, adds zram swap and moves
#     Docker to memory for a tier that asks, at boot.
#
# Settings (set by build-ec2-ami.sh):
#   RUNNER_VERSION  actions/runner version, e.g. 2.338.1
#   RUNNER_SHA256   its tarball's SHA-256, from the release notes or pinned
#   RUNNER_ARCH     x64 or arm64
#   RI_REF, RI_COMMIT  the runner-images tag and the commit fetched for it
#   TOOLSET         standard (GitHub's ubuntu-24.04 tools that jobs commonly
#                   use), full (nearly all of them) or minimal (Docker, git)
#   FR_VERSION      firerunner release for the git cache's proxy; empty means
#                   the latest
#   FR_URL          take the binary from here instead, checked against FR_SHA256
set -euxo pipefail

: "${RUNNER_VERSION:?}" "${RUNNER_SHA256:?}" "${RUNNER_ARCH:?}" "${TOOLSET:?}"

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

# What the build writes and deletes stays out of the snapshot this way: EBS keeps
# every block once written, so deleting it from the disk saves nothing.
SCRATCH="/tmp /var/tmp /var/cache/apt/archives /root/.cache /imagegeneration"
if [ "$TOOLSET" != minimal ]; then
	for dir in $SCRATCH; do
		mkdir -p "$dir"
		mount -t tmpfs -o size=60% tmpfs "$dir"
	done
	mkdir -p /var/cache/apt/archives/partial
	chmod 1777 /tmp /var/tmp
fi

export DEBIAN_FRONTEND=noninteractive
# First boot is when apt-daily and unattended-upgrades run and hold the dpkg
# lock: wait for it instead of failing at once.
apt_get() { apt-get -o DPkg::Lock::Timeout=600 "$@"; }
apt_get update
apt_get install -y --no-install-recommends \
	zstd curl jq git ca-certificates nginx gcc libc6-dev cloud-guest-utils nftables
if [ "$TOOLSET" != minimal ]; then
	install_runner_images
	# As on GitHub's runners, jobs run as "runner", with sudo and Docker.
	# A home from /etc/skel holds the tools runner-images put there.
	id runner >/dev/null 2>&1 || useradd -m -s /bin/bash runner
	usermod -aG docker runner
	# Paths /etc/environment and setup-* actions install into. GitHub's
	# configure-system.sh opens them; it does more that this image must not.
	for dir in /opt/hostedtoolcache /opt/pipx /opt/pipx_bin /usr/share/dotnet /usr/share/miniconda; do
		[ ! -e "$dir" ] || chown -R runner:runner "$dir"
	done
	printf 'runner ALL=(ALL) NOPASSWD:ALL\nDefaults:runner !requiretty, env_keep += "*"\n' >/etc/sudoers.d/runner
	chmod 0440 /etc/sudoers.d/runner
	# useradd copied these; the build's own caches are of no use to a job.
	rm -rf /etc/skel/.rustup /etc/skel/.cargo /root/.npm /root/.local
else
	apt_get install -y --no-install-recommends docker.io
fi
# zram is in the kernel's extra modules on some Ubuntu kernels.
# The build may have upgraded the kernel, so not the running one's.
apt_get install -y --no-install-recommends "linux-modules-extra-$(find /lib/modules -mindepth 1 -maxdepth 1 -printf '%f\n' | sort -V | tail -n 1)" || true

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
# firerunner-ec2-tune.sh starts Docker, after moving its data if the tier asks,
# so a boot does not start it twice. The socket is the fallback: the first docker
# command starts it, if nothing else has.
systemctl disable docker.service containerd.service
systemctl enable docker.socket

# actions/runner. GitHub refuses runner versions more than ~30 days old, so the
# image must be rebuilt monthly (see images/README.md).
install -d /opt/runner
cd /opt/runner
fetch_checked "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
	runner.tgz "$RUNNER_SHA256" || {
	mark "FAILED actions/runner $RUNNER_VERSION: download failed or its SHA-256 is not $RUNNER_SHA256"
	exit 1
}
tar xzf runner.tgz
rm runner.tgz
./bin/installdependencies.sh
[ "$TOOLSET" = minimal ] || chown -R runner:runner /opt/runner

# Boot unit and script: install_boot_unit is defined above, by build-ec2-ami.sh.
install_boot_unit

# The dependency cache proxy (#28), installed inactive: firerunner-ec2-run.sh
# runs firerunner-cache-setup only when the instance's tier caches. nginx
# starts then, not at boot.
install_cache_proxy
systemctl disable nginx.service

# The git cache's proxy, started only for a tier that has one. Best effort: an
# image without it runs every job, with git going to GitHub.
install_firerunner || mark "no firerunner binary; no git cache on this image"
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
[ "$TOOLSET" != minimal ] || apt_get purge -y --auto-remove gcc libc6-dev

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
for unit in nftables.service apt-daily.timer apt-daily-upgrade.timer man-db.timer motd-news.timer \
	fstrim.timer e2scrub_all.timer unattended-upgrades.service \
	ModemManager.service multipathd.service multipathd.socket \
	php8.3-fpm.service phpsessionclean.timer podman.service podman-restart.service \
	podman-clean-transient.service podman-auto-update.service podman-auto-update.timer \
	hibinit-agent.service networkd-dispatcher.service udisks2.service sysstat.service \
	sysstat-collect.timer sysstat-summary.timer pollinate.service open-iscsi.service \
	iscsid.socket open-vm-tools.service vgauth.service lvm2-monitor.service \
	lvm2-lvmpolld.socket dm-event.socket blk-availability.service ubuntu-advantage.service \
	ua-reboot-cmds.service grub-common.service grub-initrd-fallback.service \
	keyboard-setup.service console-setup.service setvtrgb.service secureboot-db.service \
	apport-autoreport.path apport-forward.socket update-notifier-download.timer \
	update-notifier-motd.timer dpkg-db-backup.timer logrotate.timer \
	ec2-instance-connect-harvest-hostkeys.service snapd.autoimport.service \
	snapd.core-fixup.service snapd.recovery-chooser-trigger.service; do
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
# A job's apt-get update fetches only what apt itself needs: on EC2 this took it
# from 4.7 to 2.4 s. These files also hold hooks that run after each update, and
# would start Python from a disk that loads on first read.
echo 'Acquire::Languages "none";' >/etc/apt/apt.conf.d/10apt-no-languages
rm -f /etc/apt/apt.conf.d/50appstream /etc/apt/apt.conf.d/50command-not-found \
	/etc/apt/apt.conf.d/20apt-esm-hook.conf /etc/apt/apt.conf.d/20packagekit \
	/etc/apt/apt.conf.d/99update-notifier

# Per-instance identity: an empty hostname is set from DHCP at boot, and an
# empty machine-id is regenerated, so instances do not share the builder's.
truncate -s 0 /etc/hostname
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id

apt-get clean
rm -rf /var/lib/apt/lists/* /var/log/journal/*
if [ "$TOOLSET" != minimal ]; then
	# Lazily: a daemon the build left running may hold a file there.
	# shellcheck disable=SC2086 # SCRATCH is a list of paths.
	umount -l $SCRATCH
fi
rm -rf /tmp/* /imagegeneration
mark finished
sync
shutdown -h now
