#!/usr/bin/env bash
# Unit tests for assets/firerunner-ec2-tune.sh. Stubs every system command, so
# it needs no root, no block devices and no Linux. images/test-ec2-vm.sh boots
# the real thing.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/assets/firerunner-ec2-tune.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
export STATE="$TMP/state"
fails=0
# The instance's /bin/sh is dash; macOS's sh is bash, which forgives what dash
# does not.
POSIX_SH="$(command -v dash || echo sh)"

stub() { # NAME BODY
	# shellcheck disable=SC2016 # the stub evaluates these
	printf '#!/bin/sh\necho "%s $*" >> "$STATE/calls"\n%s\n' "$1" "$2" > "$TMP/bin/$1"
	chmod +x "$TMP/bin/$1"
}
fresh() {
	rm -rf "$STATE"
	mkdir -p "$STATE"
	stub findmnt 'echo /dev/nvme0n1p1'
	stub lsblk 'echo nvme0n1'
	mkdir -p "$STATE/sys/nvme0n1p1"
	echo 1 > "$STATE/sys/nvme0n1p1/partition"
	stub sfdisk 'cat > /dev/null'
	stub partx 'exit 0'
	stub growpart 'exit 0'
	stub resize2fs 'exit 0'
	stub modprobe 'exit 0'
	stub zramctl 'echo /dev/zram0'
	stub mkswap 'exit 0'
	stub swapon 'exit 0'
	stub sysctl 'exit 0'
	stub systemctl 'exit 0'
	stub mkfs.ext4 'exit 0'
	stub mount 'exit 0'
	stub umount 'exit 0'
	printf 'MemTotal:       16000000 kB\nMemFree: 1 kB\n' > "$STATE/meminfo"
}
run() {
	rc=0
	PATH="$TMP/bin:$PATH" FIRERUNNER_MEMINFO="$STATE/meminfo" FIRERUNNER_PREHEAT_PATHS="$TMP/absent" FIRERUNNER_ZRAM_TRIES=4 FIRERUNNER_SYS_BLOCK="$STATE/sys" \
		FIRERUNNER_DOCKER_STORAGE="${DOCKER_STORAGE:-}" FIRERUNNER_DOCKER_PENDING="${DOCKER_PENDING:-}" FIRERUNNER_DOCKER_MEM_DIR="$STATE/mem" FIRERUNNER_DOCKER_DIRS="$STATE/lib/docker $STATE/lib/containerd" \
		"$POSIX_SH" "$SCRIPT" 2> "$STATE/stderr" || rc=$?
}
pass() { # DESC CMD...
	local d="$1"
	shift
	if "$@"; then echo "ok   $d"; else
		echo "FAIL $d"
		cat "$STATE/calls" "$STATE/stderr" 2> /dev/null || true
		fails=$((fails + 1))
	fi
}
not() { ! "$@"; }
called() { grep -qxF -- "$1" "$STATE/calls"; }
not_called() { ! grep -q -- "^$1" "$STATE/calls"; }

fresh
run
pass "exits 0" [ "$rc" = 0 ]
pass "asks for the canonical root device" called "findmnt -cno SOURCE /"
pass "grows the root partition" called "sfdisk --force --quiet --no-reread --no-tell-kernel -N 1 /dev/nvme0n1"
pass "tells the kernel its new size" called "partx --update --nr 1 /dev/nvme0n1"
pass "does not need growpart" not_called growpart
pass "resizes the filesystem" called "resize2fs /dev/nvme0n1p1"
pass "zram is as large as memory, with zstd" called "zramctl --find --size 16000000KiB --algorithm zstd"
pass "swap goes on the device zramctl chose" called "mkswap /dev/zram0"
pass "swap is on" called "swapon --priority 100 /dev/zram0"
pass "prefers swap to dropping page cache" called "sysctl -q vm.swappiness=100"
pass "says nothing when all went well" [ ! -s "$STATE/stderr" ]
pass "leaves Docker alone by default" not_called systemctl

# ordered A B: A's call comes before B's.
ordered() {
	local a b
	a="$(grep -nF -- "$1" "$STATE/calls" | head -n 1 | cut -d: -f1)"
	b="$(grep -nF -- "$2" "$STATE/calls" | head -n 1 | cut -d: -f1)"
	[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}

DOCKER_STORAGE=memory
fresh
run
pass "memory: exits 0" [ "$rc" = 0 ]
pass "memory: stops Docker, socket first, or it comes back" called "systemctl stop docker.socket docker.service containerd.service"
pass "memory: three times the RAM caps the device" called "zramctl --find --size 48000000KiB --algorithm zstd"
pass "memory: a filesystem on the device" called "mkfs.ext4 -q -m 0 -E nodiscard /dev/zram0"
pass "memory: mounted, handing deleted data's memory back" called "mount -o noatime,data=writeback,discard /dev/zram0 $STATE/mem"
pass "memory: docker's directory moves" called "mount --bind $STATE/mem/docker $STATE/lib/docker"
pass "memory: containerd's directory moves" called "mount --bind $STATE/mem/containerd $STATE/lib/containerd"
pass "memory: moved before Docker is back" ordered "mount --bind $STATE/mem/containerd" "systemctl start"
pass "memory: Docker comes back" called "systemctl start containerd.service docker.service"
pass "memory: says nothing when all went well" [ ! -s "$STATE/stderr" ]
pass "memory: swap keeps the first device" ordered "mkswap" "systemctl stop"
pass "memory: zram keeps swap's read-ahead" not_called "sysctl -q vm.page-cluster"
pass "memory: not on tmpfs at 16 GB" not_called "mount -t tmpfs"

big() { fresh; sed -i.bak 's/^MemTotal:.*/MemTotal: 32000000 kB/' "$STATE/meminfo"; }

big
run
pass "large: exits 0" [ "$rc" = 0 ]
pass "large: tmpfs, half as large again as memory" called "mount -t tmpfs -o noatime,size=48000000k tmpfs $STATE/mem"
pass "large: no filesystem to compress into" not_called mkfs.ext4
pass "large: only swap's device" not_called "zramctl --find --size 96000000KiB"
pass "large: swap reads one page at a time" called "sysctl -q vm.page-cluster=0"
pass "large: docker's directory moves" called "mount --bind $STATE/mem/docker $STATE/lib/docker"
pass "large: Docker comes back after the move" ordered "mount --bind $STATE/mem/containerd" "systemctl start"
pass "large: says nothing when all went well" [ ! -s "$STATE/stderr" ]

big
FIRERUNNER_DOCKER_TMPFS_MIN_KIB=32000000
export FIRERUNNER_DOCKER_TMPFS_MIN_KIB
run
pass "at the threshold: tmpfs" called "mount -t tmpfs -o noatime,size=48000000k tmpfs $STATE/mem"
FIRERUNNER_DOCKER_TMPFS_MIN_KIB=32000001
run
unset FIRERUNNER_DOCKER_TMPFS_MIN_KIB
pass "under the threshold: a filesystem" called "mkfs.ext4 -q -m 0 -E nodiscard /dev/zram0"

# Without swap, tmpfs would have nowhere to put what outgrows memory.
big
stub swapon 'exit 1'
run
pass "no swap: stays compressed" called "mkfs.ext4 -q -m 0 -E nodiscard /dev/zram0"
pass "no swap: not on tmpfs" not_called "mount -t tmpfs"

big
stub mount 'case "$*" in *tmpfs*) exit 1 ;; esac; exit 0'
run
pass "tmpfs fails: exits 0" [ "$rc" = 0 ]
pass "tmpfs fails: Docker stays on the disk" not_called "mount --bind"
pass "tmpfs fails: says so" grep -q 'could not move Docker' "$STATE/stderr"
pass "tmpfs fails: Docker comes back" called "systemctl start containerd.service docker.service"
pass "memory: Docker settles before the disk, which can stall" ordered "systemctl start" "sfdisk"

# The boot service waits on this file: it must outlive Docker's restart, and
# must be gone before the disk, which can stall, is touched.
watch_pending() {
	: > "$STATE/pending"
	# shellcheck disable=SC2016 # the stubs evaluate these
	stub systemctl 'if [ "$1" = start ] && [ -e "$STATE/pending" ]; then : > "$STATE/held-through-start"; fi'
	# shellcheck disable=SC2016
	stub sfdisk 'cat > /dev/null; [ -e "$STATE/pending" ] || : > "$STATE/gone-before-disk"'
}
fresh
watch_pending
DOCKER_PENDING="$STATE/pending" run
pass "memory: the boot service waits through Docker's restart" [ -e "$STATE/held-through-start" ]
pass "memory: the boot service does not wait for the disk" [ -e "$STATE/gone-before-disk" ]

fresh
watch_pending
stub mkfs.ext4 'exit 1'
DOCKER_PENDING="$STATE/pending" run
pass "no filesystem: the boot service waits through Docker's restart" [ -e "$STATE/held-through-start" ]
pass "no filesystem: the boot service does not wait for the disk" [ -e "$STATE/gone-before-disk" ]

fresh
stub mkfs.ext4 'exit 1'
run
pass "no filesystem: exits 0" [ "$rc" = 0 ]
pass "no filesystem: Docker comes back" called "systemctl start containerd.service docker.service"
pass "no filesystem: says so" grep -q "stays on the disk" "$STATE/stderr"
pass "no filesystem: nothing mounted" not_called mount

# The second directory fails: the first must not stay moved.
fresh
# shellcheck disable=SC2016 # the stub evaluates this
stub mount 'case "$*" in *containerd*) exit 1 ;; esac; exit 0'
run
pass "half moved: undoes the first" called "umount $STATE/lib/docker"
pass "half moved: undoes the mount" called "umount $STATE/mem"
pass "half moved: Docker comes back after the undo" ordered "umount $STATE/mem" "systemctl start"
pass "half moved: says so" grep -q "stays on the disk" "$STATE/stderr"

# Undone in full, Docker is on the disk, and the job can run on it.
fresh
: > "$STATE/pending"
# shellcheck disable=SC2016 # the stub evaluates this
stub mount 'case "$*" in *containerd*) exit 1 ;; esac; exit 0'
DOCKER_PENDING="$STATE/pending" run
pass "half moved, undone: the boot service may run the job" [ ! -e "$STATE/pending" ]

# Not undone, dockerd and containerd are on different filesystems.
fresh
: > "$STATE/pending"
# shellcheck disable=SC2016 # the stub evaluates this
stub mount 'case "$*" in *containerd*) exit 1 ;; esac; exit 0'
stub umount 'exit 1'
DOCKER_PENDING="$STATE/pending" run
pass "half moved, not undone: exits 0" [ "$rc" = 0 ]
pass "half moved, not undone: the boot service must not run the job" [ -e "$STATE/pending" ]

fresh
# shellcheck disable=SC2016 # the stub evaluates this
stub systemctl 'case "$1" in stop) exit 1 ;; esac; exit 0'
run
pass "Docker will not stop: exits 0" [ "$rc" = 0 ]
pass "Docker will not stop: its data stays" not_called mkfs.ext4
pass "Docker will not stop: says so" grep -q "stays on the disk" "$STATE/stderr"
pass "Docker will not stop: asks it back, as a stop that timed out still happens" called "systemctl start containerd.service docker.service"

fresh
# shellcheck disable=SC2016 # the stub evaluates this
stub systemctl 'case "$1" in start) exit 1 ;; esac; exit 0'
run
pass "Docker will not start: exits 0" [ "$rc" = 0 ]
pass "Docker will not start: says that, not that it stayed" grep -q "could not start Docker" "$STATE/stderr"
pass "Docker will not start: does not claim the disk" not grep -q "stays on the disk" "$STATE/stderr"

fresh
: > "$STATE/pending"
# shellcheck disable=SC2016 # the stub evaluates this
stub systemctl 'case "$1" in start) exit 1 ;; esac; exit 0'
DOCKER_PENDING="$STATE/pending" run
pass "Docker will not start: the boot service must not run the job" [ -e "$STATE/pending" ]
pass "Docker will not start: the disk still grows" called "resize2fs /dev/nvme0n1p1"
DOCKER_STORAGE=

fresh
stub sfdisk 'cat > /dev/null; exit 1'
run
pass "sfdisk fails: falls back to growpart" called "growpart /dev/nvme0n1 1"

fresh
stub partx 'exit 1'
run
pass "partx fails: falls back to growpart" called "growpart /dev/nvme0n1 1"

# growpart exits 1 when the disk is already grown (a pool instance's job boot).
fresh
stub sfdisk 'cat > /dev/null; exit 1'
stub growpart 'exit 1'
run
pass "already grown: still exits 0" [ "$rc" = 0 ]
pass "already grown: resize2fs still runs" called "resize2fs /dev/nvme0n1p1"
pass "already grown: no complaint" [ ! -s "$STATE/stderr" ]

fresh
stub resize2fs 'exit 1'
run
pass "resize fails: exits 0, the job still runs" [ "$rc" = 0 ]
pass "resize fails: says so" grep -q "could not grow" "$STATE/stderr"
pass "resize fails: swap still goes on" called "swapon --priority 100 /dev/zram0"

fresh
stub zramctl 'exit 1'
run
pass "no zram: exits 0" [ "$rc" = 0 ]
pass "no zram: says so" grep -q "could not set up zram" "$STATE/stderr"
pass "no zram: no mkswap on nothing" not_called mkswap

# udev holds a new zram device briefly: the first attempts fail busy.
fresh
# shellcheck disable=SC2016 # the stub evaluates these
stub zramctl 'n=$(cat "$STATE/busy" 2>/dev/null || echo 0); echo $((n + 1)) > "$STATE/busy"; [ "$n" -ge 2 ] || exit 1; echo /dev/zram0'
run
pass "busy zram device: retried until it is free" called "swapon --priority 100 /dev/zram0"
pass "busy zram device: no complaint" [ ! -s "$STATE/stderr" ]

# The partition comes from the kernel, not from digits in the device's name.
fresh
stub findmnt 'echo /dev/mapper/root2'
stub lsblk 'echo nvme1n1'
mkdir -p "$STATE/sys/root2"
echo 7 > "$STATE/sys/root2/partition"
run
pass "partition number comes from the kernel" called "sfdisk --force --quiet --no-reread --no-tell-kernel -N 7 /dev/nvme1n1"

# growpart refuses a padded number, which lsblk's PARTN would give.
fresh
stub sfdisk 'cat > /dev/null; exit 1'
run
pass "the fallback gets a bare partition number" called "growpart /dev/nvme0n1 1"

fresh
stub findmnt 'exit 1'
run
pass "no root device: exits 0" [ "$rc" = 0 ]
pass "no root device: no growpart" not_called growpart

# The preheat reads files in the background and holds nothing up.
fresh
mkdir -p "$TMP/warm"
echo data > "$TMP/warm/file"
# shellcheck disable=SC2016 # the stub evaluates these
stub xargs 'cat > "$STATE/preheat-list"; : > "$STATE/preheated"'
PATH="$TMP/bin:$PATH" FIRERUNNER_MEMINFO="$STATE/meminfo" FIRERUNNER_SYS_BLOCK="$STATE/sys" FIRERUNNER_PREHEAT_PATHS="$TMP/warm" "$POSIX_SH" "$SCRIPT" 2> "$STATE/stderr"
for _ in $(seq 1 100); do
	[ -f "$STATE/preheated" ] && break
	sleep 0.05
done
pass "preheat reads the listed files" grep -q "warm/file" "$STATE/preheat-list"

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
