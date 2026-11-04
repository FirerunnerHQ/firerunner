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
	printf 'MemTotal:       16000000 kB\nMemFree: 1 kB\n' > "$STATE/meminfo"
}
run() {
	rc=0
	PATH="$TMP/bin:$PATH" FIRERUNNER_MEMINFO="$STATE/meminfo" FIRERUNNER_PREHEAT_PATHS="$TMP/absent" FIRERUNNER_ZRAM_TRIES=4 FIRERUNNER_SYS_BLOCK="$STATE/sys" \
		sh "$SCRIPT" 2> "$STATE/stderr" || rc=$?
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
PATH="$TMP/bin:$PATH" FIRERUNNER_MEMINFO="$STATE/meminfo" FIRERUNNER_SYS_BLOCK="$STATE/sys" FIRERUNNER_PREHEAT_PATHS="$TMP/warm" sh "$SCRIPT" 2> "$STATE/stderr"
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
