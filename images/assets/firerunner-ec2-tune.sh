#!/bin/sh
#
# firerunner-ec2-tune.sh — readies a job's EC2 instance while the boot service
# waits for its config. The disk and the instance last for one job, so this
# favours speed. Each step is best effort: a job on a smaller disk, without
# swap, or with Docker on the disk is better than no job.
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
# The runner first: a job waits for it, and Docker's binaries can follow.
PREHEAT_PATHS="${FIRERUNNER_PREHEAT_PATHS:-/opt/runner/bin /opt/runner/externals /usr/local/bin/firerunner /usr/bin/git /usr/bin/dockerd /usr/bin/containerd /usr/bin/docker /usr/bin/runc /usr/libexec/docker}"
# "memory" keeps Docker's data in compressed RAM; set from the tier's
# docker_storage by firerunner-ec2-run.sh.
DOCKER_STORAGE="${FIRERUNNER_DOCKER_STORAGE:-}"
DOCKER_DIRS="${FIRERUNNER_DOCKER_DIRS:-/var/lib/docker /var/lib/containerd}"
DOCKER_MEM_DIR="${FIRERUNNER_DOCKER_MEM_DIR:-/run/firerunner-docker}"
# From here up, Docker's data goes on tmpfs. It is below a 32 GiB instance's
# MemTotal, and above 16 GiB's, where tmpfs saved no time.
DOCKER_TMPFS_MIN_KIB="${FIRERUNNER_DOCKER_TMPFS_MIN_KIB:-29360128}"
# Removed once Docker is back in a known state, for the boot service to wait
# on. Left in place, it tells the boot service not to run the job.
DOCKER_PENDING="${FIRERUNNER_DOCKER_PENDING:-}"

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

# zram_device KIB sets dev to a new zram device of that size.
zram_device() {
	modprobe zram 2>/dev/null || true
	# udev holds a new device for a moment after modprobe: zramctl then fails
	# with "Device or resource busy".
	tries=0
	until dev="$(zramctl --find --size "${1}KiB" --algorithm zstd 2>/dev/null)"; do
		tries=$((tries + 1))
		[ "$tries" -lt "$ZRAM_TRIES" ] || return 1
		sleep 0.2
	done
}

mem_kib() {
	sed -n 's/^MemTotal: *\([0-9]*\) kB/\1/p' "$MEMINFO"
}

# Swap in RAM, so a job that briefly needs more memory than the instance has
# slows down instead of being killed. It does not touch the slower disk.
zram_swap() {
	kib="$(mem_kib)"
	[ -n "$kib" ] || return 1
	zram_device "$kib" || return 1
	mkswap "$dev" >/dev/null || return 1
	swapon --priority 100 "$dev" || return 1
	sysctl -q vm.swappiness=100 || true
	swap_up=1
}

# Moves Docker's data to RAM, compressed if it must be, for a tier that asks. A large image
# build took 30% less time than on 3000 IOPS of gp3, because EBS stops being read
# as well as written. Docker starts empty there: images in the AMI stay on the
# disk, out of its sight. The size only caps the device: zram takes memory as
# data arrives, about 1 GiB for 2.5 GiB of it, and gives it back on discard. The
# instance's memory is the real limit, so a build that outgrows it fails where
# a disk would have held it: this is a tier's choice, never a default.
docker_memory() {
	moved=1
	kib="$(mem_kib)"
	if [ -n "$kib" ] && bounded systemctl stop docker.socket docker.service containerd.service; then
		docker_memory_mount "$kib" && moved=0
	fi
	# Whatever happened: a stop that failed or timed out may still stop them.
	# Not waited for: Docker's data is in place, and the runner need not wait
	# for dockerd, which the first docker command waits on through the socket.
	if ! bounded systemctl start --no-block containerd.service docker.service; then
		log "could not start Docker"
		docker_unsettled=1
	fi
	return "$moved"
}

docker_memory_mount() {
	mkdir -p "$DOCKER_MEM_DIR" || return 1
	# With memory to spare, tmpfs compresses only what swap takes, so writes cost
	# no CPU: a large image build's biggest COPY took 3 s, against 14 s on zram.
	# At 16 GiB memory runs out, so it compresses all the same and saves nothing.
	# It needs swap, or it has no way to outgrow memory.
	if [ "$1" -ge "$DOCKER_TMPFS_MIN_KIB" ] && [ -n "$swap_up" ]; then
		sysctl -q vm.page-cluster=0 || true
		bounded mount -t tmpfs -o "noatime,size=$(($1 * 3 / 2))k" tmpfs "$DOCKER_MEM_DIR" || return 1
	else
		zram_device "$(($1 * 3))" || return 1
		bounded mkfs.ext4 -q -m 0 -E nodiscard "$dev" || return 1
		bounded mount -o noatime,data=writeback,discard "$dev" "$DOCKER_MEM_DIR" || return 1
	fi
	bound=""
	for dir in $DOCKER_DIRS; do
		if ! mkdir -p "$DOCKER_MEM_DIR/${dir##*/}" "$dir" ||
			! bounded mount --bind "$DOCKER_MEM_DIR/${dir##*/}" "$dir"; then
			# Half moved would split containerd's data from dockerd's.
			for undo in $bound; do
				bounded umount "$undo" || docker_unsettled=1
			done
			bounded umount "$DOCKER_MEM_DIR" || true
			return 1
		fi
		bound="$bound $dir"
	done
}

# Detached: reading must not hold the job back.
preheat() {
	# shellcheck disable=SC2086 # PREHEAT_PATHS is a list of paths.
	# Alpine's node runs only in an Alpine job container, not here. In parallel,
	# because EBS loads a snapshot faster at a higher queue depth.
	nohup sh -c "find $PREHEAT_PATHS -xdev -type f ! -path '*_alpine*' -print0 2>/dev/null | xargs -0 -r -P 4 -n 32 cat >/dev/null 2>&1" >/dev/null 2>&1 &
}

docker_unsettled=""
swap_up=""
preheat
zram_swap || log "could not set up zram swap; the job runs without swap"
# Before the disk, which can stall: the job waits for this step, not for that.
if [ "$DOCKER_STORAGE" = memory ]; then
	docker_memory || log "could not move Docker to memory; its data stays on the disk"
else
	# Not waited for: the first docker command waits on the socket if it must.
	systemctl start --no-block containerd.service docker.service || log "could not start Docker"
fi
[ -n "$docker_unsettled" ] || [ -z "$DOCKER_PENDING" ] || rm -f "$DOCKER_PENDING"
grow_root || log "could not grow the root filesystem; the job keeps the image's size"
exit 0
