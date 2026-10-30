#!/bin/sh
#
# firerunner-ec2-run.sh — EC2 runner AMI boot service body. Fetches this
# instance's JIT config from the firerunner control VM over the VPC, runs
# exactly one ephemeral GitHub Actions job, then powers off.
#
# User-data holds only the endpoint (FIRERUNNER_JIT_URL=http://<vpc-ip>:<port>/jit),
# never the config itself: the control VM hands the config out once, to this
# instance's private IP, and answers 404 afterwards. So neither user-data nor
# IMDS holds anything reusable. The job itself runs as root alongside the
# runner and can read the config the runner was started with; it is single-use
# and dies with the instance.
#
# Every wait below is a retry loop rather than a systemd ordering dependency, so
# the fetch starts as soon as the NIC has an address instead of waiting for
# network-online.target or docker.
#
# Powering off ends the instance: it is launched with shutdown behaviour
# "terminate". A pooled persistent-spot instance can only stop, and firerunner
# terminates it.
#
# Once user-data names an endpoint, this boot is for a job and the disk is
# thrown away after it either way, so it powers off without stopping units
# (POWEROFF_FAST): the instance is billed until it is shutting down, and with a
# clean stop of docker, containerd and the rest, 10-12 s passed between the
# job completing and the instance shutting down. Processes are still
# killed and the file systems unmounted or remounted read-only. Any other boot
# (a pool instance's warm-up, an image build, a failure before user-data is
# read) keeps the clean POWEROFF: that disk may boot again.
set -eu

IMDS="${FIRERUNNER_IMDS:-169.254.169.254}"
RUNNER_DIR="${FIRERUNNER_RUNNER_DIR:-/opt/runner}"
POWEROFF="${FIRERUNNER_POWEROFF:-systemctl poweroff}"
POWEROFF_FAST="${FIRERUNNER_POWEROFF_FAST:-systemctl poweroff --force}"
poweroff_cmd="$POWEROFF"
RETRY_SLEEP="${FIRERUNNER_RETRY_SLEEP:-0.2}"
# Give up on a config that never arrives; firerunner revokes it long before.
JIT_WAIT="${FIRERUNNER_JIT_WAIT:-600}"
# Bounds each progress report to the control VM.
REPORT_TIMEOUT="${FIRERUNNER_REPORT_TIMEOUT:-2}"
# Read on a warm-up boot, so the first job does not pay for EBS fetching their
# blocks from the snapshot.
WARM_PATHS="${FIRERUNNER_WARM_PATHS:-$RUNNER_DIR /usr/bin/dockerd /usr/bin/containerd /usr/bin/docker /usr/bin/runc}"

log() { echo "firerunner: $*" >&2; }

finish() {
	sync
	# shellcheck disable=SC2086 # poweroff_cmd is a command line.
	$poweroff_cmd
}
trap finish EXIT

# IMDSv2 session token. IMDS can lag the first moments of boot.
until tok="$(curl -sf -m 2 -X PUT "http://$IMDS/latest/api/token" \
	-H "X-aws-ec2-metadata-token-ttl-seconds: 300")"; do
	sleep "$RETRY_SLEEP"
done

ud="$(curl -sf -m 2 -H "X-aws-ec2-metadata-token: $tok" "http://$IMDS/latest/user-data" || true)"
url="$(printf '%s\n' "$ud" | sed -n 's/^FIRERUNNER_JIT_URL=//p' | head -n 1)"
if [ -z "$url" ]; then
	# A pool instance's warm-up boot (or an image build): no job. Read the
	# files the first job needs, so EBS has their blocks, then power off; the
	# instance stops until a job claims it.
	log "no FIRERUNNER_JIT_URL in user-data: warm-up boot; reading the runner's files, then powering off"
	# shellcheck disable=SC2086 # WARM_PATHS is a list of paths.
	find $WARM_PATHS -xdev -type f -print0 2>/dev/null | xargs -0 cat >/dev/null 2>&1 || true
	exit 0
fi
# A job boot from here on: its disk is never booted again.
poweroff_cmd="$POWEROFF_FAST"

body="$(mktemp)"
trap 'rm -f "$body"; finish' EXIT
start="$(date +%s)"
while :; do
	code="$(curl -s -m 5 -o "$body" -w '%{http_code}' "$url" || true)"
	case "$code" in
	200)
		break
		;;
	404)
		# Already served (to an earlier attempt) or expired: it will never come.
		log "JIT config already claimed or expired; powering off"
		exit 0
		;;
	esac
	# 403 means firerunner has not registered this address yet; anything else
	# is the network still coming up.
	if [ $(($(date +%s) - start)) -ge "$JIT_WAIT" ]; then
		log "no JIT config after ${JIT_WAIT}s (last status ${code:-none}); powering off"
		exit 0
	fi
	sleep "$RETRY_SLEEP"
done
jit="$(cat "$body")"
rm -f "$body"
if [ -z "$jit" ]; then
	log "empty JIT config; powering off"
	exit 0
fi

# report PHASE [RC]: tell the control VM how the runner is doing (see
# internal/jitserver: listening, job, exited with its status), so a runner that
# fails backs firerunner off instead of being relaunched at once, and boot time
# splits into phases. Best effort: a report that fails changes nothing here.
report() {
	q="phase=$1"
	[ $# -lt 2 ] || q="$q&rc=$2"
	curl -s -m "$REPORT_TIMEOUT" -o /dev/null -X POST "$url/report?$q" || true
}

if ! cd "$RUNNER_DIR" 2>/dev/null || [ ! -x ./run.sh ]; then
	log "no runnable $RUNNER_DIR/run.sh; powering off"
	report exited 127
	exit 0
fi
# The instance is single-use and thrown away after the job.
export RUNNER_ALLOW_RUNASROOT=1
export HOME=/root
export USER=root

# The runner's output goes through a FIFO to a reader that passes it on to the
# journal and reports the phases it shows. A FIFO, not a pipeline: this script
# waits for the runner alone, never for whatever else holds its output open (a
# daemon a job step left behind).
fifo="$(mktemp -u)"
mkfifo "$fifo"
while IFS= read -r line; do
	printf '%s\n' "$line"
	case "$line" in
	*"Listening for Jobs"*) report listening ;;
	*"Running job:"*) report job ;;
	esac
done <"$fifo" &
# Ephemeral JIT runner: registers, runs one job, then auto-deregisters.
rc=0
./run.sh --jitconfig "$jit" >"$fifo" 2>&1 || rc=$?
rm -f "$fifo"
[ "$rc" -eq 0 ] || log "runner exited with status $rc"
report exited "$rc"
