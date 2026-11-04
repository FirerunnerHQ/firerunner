#!/bin/sh
#
# firerunner-ec2-tune.sh — readies a job's EC2 instance while the boot service
# waits for its config. The disk and the instance last for one job, so this
# favours speed. Each step is best effort: a job on a smaller disk, or without
# swap, is better than no job.
#
# It runs on every job boot, so it must be safe on a disk an earlier boot has
# grown (a pool instance boots once before its job).
set -u

MEMINFO="${FIRERUNNER_MEMINFO:-/proc/meminfo}"
# Bounds a stuck disk or device, so the job still starts.
STEP_TIMEOUT="${FIRERUNNER_TUNE_TIMEOUT:-30}"
ZRAM_TRIES="${FIRERUNNER_ZRAM_TRIES:-15}"
SYS_BLOCK="${FIRERUNNER_SYS_BLOCK:-/sys/class/block}"
# Read in the background, so a job's first docker command does not pay for EBS
# fetching their blocks from the snapshot.
PREHEAT_PATHS="${FIRERUNNER_PREHEAT_PATHS:-/usr/bin/dockerd /usr/bin/containerd /usr/bin/docker /usr/bin/runc /usr/libexec/docker /opt/runner/bin /opt/runner/externals}"

log() { echo "firerunner-tune: $*" >&2; }

bounded() {
	if command -v timeout >/dev/null 2>&1; then
		timeout "$STEP_TIMEOUT" "$@"
	else
		"$@"
	fi
}

# The image's filesystem is smaller than a tier's root_volume_gib, and
# cloud-init, which would grow it, is off for a faster boot.
grow_root() {
	# Canonical, because a kernel booted without an initramfs reports /dev/root.
	src="$(findmnt -cno SOURCE /)" || return 1
	disk="$(lsblk -no PKNAME "$src")" || return 1
	# From the kernel: lsblk's PARTN needs udev's database, which may not be
	# ready this early, and pads the number, which growpart refuses.
	part="$(cat "$SYS_BLOCK/${src##*/}/partition")" || return 1
	[ -n "$disk" ] && [ -n "$part" ] || return 1
	# sfdisk also moves a GPT backup header to the disk's new end, and takes
	# about 40 ms where growpart takes a second or two at boot.
	if ! printf ',+\n' | bounded sfdisk --force --quiet --no-reread --no-tell-kernel -N "$part" "/dev/$disk" >/dev/null 2>&1 ||
		! bounded partx --update --nr "$part" "/dev/$disk" >/dev/null 2>&1; then
		# It exits 1 for "nothing to do", as it does for failure: resize2fs is
		# safe either way, and says which it was.
		bounded growpart "/dev/$disk" "$part" >/dev/null 2>&1 || true
	fi
	bounded resize2fs "$src" >/dev/null 2>&1
}

# Swap in RAM, so a job that briefly needs more memory than the instance has
# slows down instead of being killed. It does not touch the slower disk.
zram_swap() {
	kib="$(sed -n 's/^MemTotal: *\([0-9]*\) kB/\1/p' "$MEMINFO")"
	[ -n "$kib" ] || return 1
	modprobe zram 2>/dev/null || true
	# udev holds a new device for a moment after modprobe: zramctl then fails
	# with "Device or resource busy".
	tries=0
	until dev="$(zramctl --find --size "${kib}KiB" --algorithm zstd 2>/dev/null)"; do
		tries=$((tries + 1))
		[ "$tries" -lt "$ZRAM_TRIES" ] || return 1
		sleep 0.2
	done
	mkswap "$dev" >/dev/null || return 1
	swapon --priority 100 "$dev" || return 1
	sysctl -q vm.swappiness=100 || true
}

# Detached: reading must not hold the job back.
preheat() {
	# shellcheck disable=SC2086 # PREHEAT_PATHS is a list of paths.
	nohup sh -c "find $PREHEAT_PATHS -xdev -type f -print0 2>/dev/null | xargs -0 -r cat >/dev/null 2>&1" >/dev/null 2>&1 &
}

preheat
grow_root || log "could not grow the root filesystem; the job keeps the image's size"
zram_swap || log "could not set up zram swap; the job runs without swap"
exit 0
