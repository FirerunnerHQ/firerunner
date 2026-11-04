#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2086 # probes and QEMU options are written literally
# test-ec2-vm.sh — builds the EC2 runner image in a KVM guest and boots it as
# a job's instance, with no AWS. It runs the real provision.sh, on Ubuntu's
# 24.04 cloud image, then boots the result on a larger disk against a fake
# IMDS and JIT endpoint. The runner is replaced by a probe that reports what a
# job would see. Needs a Linux host with /dev/kvm, qemu, python3 and socat.
#
# Usage: images/test-ec2-vm.sh [--workdir DIR] [--runner-version V] [--vcpus N]
#                              [--mem-mib N] [--disk-gib N]
#                              [--keep-image FILE]
#
# The image is what EC2 would snapshot, except for its kernel: this is Ubuntu's
# generic kernel, where EC2 has the aws one. Timings here say nothing about EBS.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
workdir="${HOME}/.cache/firerunner-ec2-vm" runner_version="" vcpus=4 mem=8192 disk=20 keep=""
while [ $# -gt 0 ]; do
	case "$1" in
	--workdir) workdir="${2:?}" && shift ;;
	--runner-version) runner_version="${2:?}" && shift ;;
	--vcpus) vcpus="${2:?}" && shift ;;
	--mem-mib) mem="${2:?}" && shift ;;
	--disk-gib) disk="${2:?}" && shift ;;
	--keep-image) keep="${2:?}" && shift ;;
	*) echo "unknown argument: $1" >&2 && exit 2 ;;
	esac
	shift
done
[ -w /dev/kvm ] || { echo "needs /dev/kvm" >&2; exit 2; }
for t in qemu-system-x86_64 qemu-img python3 socat curl; do
	command -v "$t" >/dev/null || { echo "needs $t" >&2; exit 2; }
done
[ "$(id -u)" != 0 ] || echo "note: running as root" >&2
[ -n "$runner_version" ] || runner_version="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"].lstrip("v"))')"

mkdir -p "$workdir"
# Absolute: qemu-img resolves a relative backing file against its overlay.
workdir="$(cd "$workdir" && pwd)"
run="$(mktemp -d "$workdir/run.XXXXXX")"
server_pid=""
qemu_pid=""
cleanup() {
	[ -z "$qemu_pid" ] || kill "$qemu_pid" 2>/dev/null || true
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	rm -f "$run"/*.qcow2
}
trap cleanup EXIT

base="$workdir/noble-server-cloudimg-amd64.img"
[ -f "$base" ] || curl -fsSL -o "$base" https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img

# The test's probe replaces the runner, so the job boot reports and powers off.
cat >"$run/probe.sh" <<'PROBE'
#!/bin/bash
# Reports as it goes, so a probe that dies still says how far it got.
url="${FIRERUNNER_RESULT_URL:?}"
say() { printf '%s\n' "$1" >>/tmp/result; curl -s -m 10 --data-binary @/tmp/result "$url"; }
say "cmdline: $(cat /proc/cmdline)"
say "user: $(id -un) home=$HOME"
say "rootmount: $(findmnt -no OPTIONS /)"
say "rootsize_gib: $(df -BG --output=size / | tail -1 | tr -dc 0-9)"
say "disk_used_gib: $(df -BG --output=used / | tail -1 | tr -dc 0-9)"
say "swap: $(swapon --noheadings --show=NAME,SIZE | tr -s ' ')"
say "swappiness: $(cat /proc/sys/vm/swappiness)"
say "thp: $(cat /sys/kernel/mm/transparent_hugepage/enabled)"
say "apt: $(apt-config dump | grep -E 'Dir::Bin::dpkg|CompressionTypes::Order' | tr '\n' ' ')"
say "memtotal_kib: $(awk '/MemTotal/{print $2}' /proc/meminfo)"
say "boot: $(systemd-analyze time 2>&1 | head -1)"
for t in gcc make unzip node npm python3 pip3 java go rustc cargo ruby gh aws az kubectl yq; do
	say "tool $t: $(command -v "$t" || echo MISSING)"
done
say "toolcache: $(ls "${AGENT_TOOLSDIRECTORY:-/opt/hostedtoolcache}" 2>/dev/null | tr '\n' ' ')"
sudo -n true 2>/dev/null && say "sudo: ok"
s=$(date +%s.%N)
timeout 120 docker run --rm hello-world >/dev/null 2>&1
rc=$?
say "docker_first_run: rc=$rc $(awk "BEGIN{print $(date +%s.%N)-$s}") s"
# A burst above physical memory must swap, not be killed. firerunner-tune
# gives swap as large as memory, so this fits.
timeout 180 python3 -c '
total = int(open("/proc/meminfo").readline().split()[1]) * 1024
chunk, chunks, n = 64 << 20, [], 0
while n < total * 1.2:
    chunks.append(bytes([n >> 26 & 255]) * chunk)
    n += chunk
print(len(chunks))
' >/dev/null 2>&1
say "overcommit_rc: $?"
say "swap_used: $(free -m | awk '/Swap/{print $3}') MiB"
# Past swap as well: the process is killed, and the job's script lives on.
timeout 120 python3 -c '
import itertools
b = []
for i in itertools.count():
    b.append(bytes(range(256)) * (1 << 20 >> 8) * 1 + __import__("os").urandom(1 << 20))
' >/dev/null 2>&1
say "oom_rc: $?"
say "probe: done"
PROBE

base_url="http://169.254.0.2:8099"
cat >"$run/server.py" <<'PY'
import http.server, os, sys
run, host = sys.argv[1], sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, body=b"", code=200):
        self.send_response(code); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_PUT(self): self.send(b"token")
    def do_GET(self):
        p = self.path.split("?")[0]
        if p == "/latest/user-data": self.send(open(run + "/job-userdata", "rb").read())
        elif p == "/user-data": self.send(open(run + "/user-data", "rb").read())
        elif p == "/meta-data": self.send(b"instance-id: firerunner-build\n")
        elif p == "/vendor-data": self.send(b"")
        elif p == "/jit": self.send(b"dummy-jit-config")
        elif p == "/probe": self.send(open(run + "/probe.sh", "rb").read())
        else: self.send(b"", 404)
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0)); body = self.rfile.read(n)
        if self.path.startswith("/result"):
            open(run + "/result", "wb").write(body)
        self.send()
http.server.ThreadingHTTPServer(("127.0.0.1", 18099), H).serve_forever()
PY
echo "FIRERUNNER_JIT_URL=$base_url/jit" >"$run/job-userdata"
python3 -I "$run/server.py" "$run" "$base_url" &
server_pid=$!

# build-ec2-ami.sh's own user-data, plus the probe in the runner's place.
"$HERE/build-ec2-ami.sh" --render --runner-version "$runner_version" >"$run/user-data.orig"
python3 -I - "$run" "$base_url" <<'PY'
import sys
run, url = sys.argv[1], sys.argv[2]
s = open(run + "/user-data.orig").read()
# The runner's place: whatever the test's server says to run.
hook = "cat >/opt/runner/run.sh <<'FIRERUNNER_PROBE'\n#!/bin/bash\nb=\"${FIRERUNNER_RESULT_URL%/result}\"\ncurl -fsS -m 20 -o /tmp/probe \"$b/probe\" && exec bash /tmp/probe\nFIRERUNNER_PROBE\nchmod +x /opt/runner/run.sh\n"
hook += "sed -i '/^ExecStart=/i Environment=FIRERUNNER_RESULT_URL=%s/result' /etc/systemd/system/firerunner-ec2-runner.service\n" % url
hook += "apt-get install -y --no-install-recommends python3 || true\n"
marker = "apt-get clean\n"
assert s.count(marker) == 1
open(run + "/user-data", "w").write(s.replace(marker, hook + marker))
PY

boot() { # DISK LOG EXTRA...
	local disk="$1" log="$2"
	shift 2
	qemu-system-x86_64 -enable-kvm -cpu host -smp "$vcpus" -m "$mem" -nographic -no-reboot \
		-drive "file=$disk,format=qcow2,if=none,id=d,discard=unmap" -device virtio-blk-pci,drive=d,bootindex=0 \
		-netdev "user,id=n,net=169.254.0.0/16,host=169.254.0.1,dhcpstart=169.254.0.15,dns=169.254.0.3,guestfwd=tcp:169.254.169.254:80-cmd:socat - TCP:127.0.0.1:18099,guestfwd=tcp:169.254.0.2:8099-cmd:socat - TCP:127.0.0.1:18099" \
		-device virtio-net-pci,netdev=n,romfile= "$@" </dev/null >"$log" 2>&1 &
	qemu_pid=$!
	local deadline=$((SECONDS + ${BOOT_DEADLINE:-1500}))
	while kill -0 "$qemu_pid" 2>/dev/null; do
		[ "$SECONDS" -lt "$deadline" ] || { echo "VM timed out, see $log" >&2; tail -20 "$log" >&2; exit 1; }
		sleep 2
	done
	wait "$qemu_pid" || true
	qemu_pid=""
}

echo ">> building the image (runner $runner_version)"
qemu-img create -q -f qcow2 -F qcow2 -b "$base" "$run/image.qcow2" "${disk}G"
boot "$run/image.qcow2" "$run/build.log" -smbios "type=1,serial=ds=nocloud;s=http://169.254.0.2:8099/"
grep -q 'FIRERUNNER-BUILD .* finished' "$run/build.log" || { echo "build failed, see $run/build.log" >&2; tail -30 "$run/build.log" >&2; exit 1; }

[ -z "$keep" ] || qemu-img convert -O qcow2 "$run/image.qcow2" "$keep"

echo ">> booting a job on an 80 GiB disk"
qemu-img create -q -f qcow2 -F qcow2 -b "$run/image.qcow2" "$run/job.qcow2" "$((disk + 60))G"
start=$SECONDS
BOOT_DEADLINE=600 boot "$run/job.qcow2" "$run/job.log"
echo "job boot to poweroff: $((SECONDS - start)) s"
[ -f "$run/result" ] || { echo "no result, see $run/job.log" >&2; tail -30 "$run/job.log" >&2; exit 1; }
cat "$run/result"

fails=0
check() { # DESC GREP-PATTERN
	if grep -Eq -- "$2" "$run/result"; then echo "ok   $1"; else
		echo "FAIL $1"
		fails=$((fails + 1))
	fi
}
check "ext4 has the job's options" 'rootmount:.*data=writeback'
check "journal commits async" 'rootmount:.*journal_async_commit'
check "write barriers off" 'rootmount:.*nobarrier'
[ "$disk" != 20 ] || check "root filesystem grew past 70 GiB" 'rootsize_gib: ([7-9][0-9])'
check "zram swap is on" 'swap: /dev/zram0'
check "swappiness is 100" 'swappiness: 100'
check "docker runs on the tuned disk" 'docker_first_run: rc=0'
check "huge pages are always on" 'thp: \[always\]'
check "dpkg runs under eatmydata" 'apt:.*dpkg-eatmydata'
check "apt prefers gzip indexes" 'apt:.*"gz"'
check "the kernel skips the PS/2 probe" 'cmdline:.*i8042.nokbd'
check "the probe finished" 'probe: done'
check "more memory than the VM has does not kill the job" 'overcommit_rc: 0'
check "a job's OOM does not take the boot service with it" 'oom_rc: 137'
if [ "$fails" != 0 ]; then
	# Keep the disks and logs, not the server, which a next run would meet on
	# its port and report to.
	kill "$server_pid" 2>/dev/null || true
	wait "$server_pid" 2>/dev/null || true
	echo "kept: $run" >&2
	trap - EXIT
	exit 1
fi
rm -rf "$run"
echo "all passed"
