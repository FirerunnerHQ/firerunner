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
#
# systemd starts this with SIGPIPE ignored (IgnoreSIGPIPE=, on by default), so
# a write to a broken journal stream fails with EPIPE rather than killing the
# shell: under set -e the script exits, and the EXIT trap still reports and
# powers off. Report before logging, so a failed log line costs no status.
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

url=""
# fetched is set once the config is in hand; exit_reported once the runner's
# exit has been reported (see report).
fetched=""
exit_reported=""

# report PHASE [RC]: tell the control VM how the runner is doing (see
# internal/jitserver: listening, job, exited with its status), so a runner that
# fails backs firerunner off instead of being relaunched at once, and boot time
# splits into phases. Best effort: a report that fails changes nothing here.
report() {
	q="phase=$1"
	[ $# -lt 2 ] || q="$q&rc=$2"
	[ "$1" != exited ] || exit_reported=1
	curl -s -m "$REPORT_TIMEOUT" -o /dev/null -X POST "$url/report?$q" || true
}

# finish STATUS runs on every exit but a signal's. Once the config is in
# hand, an exit that has not reported the runner's status reports a failure
# (STATUS, or 1 when STATUS is 0): this boot was meant to run a job and did
# not get to.
finish() {
	if [ -n "$fetched" ] && [ -z "$exit_reported" ]; then
		report exited "$([ "$1" -ne 0 ] && echo "$1" || echo 1)"
	fi
	sync
	# shellcheck disable=SC2086 # poweroff_cmd is a command line.
	$poweroff_cmd
}
trap 'finish $?' EXIT
# A signal is the instance going away (shutdown, a spot interruption, a
# terminate): it is not the runner failing, so exit with no report and leave
# the poweroff to whoever is stopping it. Trapped explicitly because shells
# differ on whether a signal runs the EXIT trap.
trap 'trap - EXIT; exit 143' TERM INT HUP

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
trap 'rc=$?; rm -f "$body"; finish "$rc"' EXIT
start="$(date +%s)"
while :; do
	code="$(curl -s -m 5 -o "$body" -w '%{http_code}' "$url" || true)"
	case "$code" in
	200)
		break
		;;
	404)
		# Already served (to an earlier attempt) or expired: it will never come.
		# Served to this address and lost on the way, the config was spent on
		# a runner that never ran: report it (firerunner ignores the report
		# from an address it never served).
		log "JIT config already claimed or expired; powering off"
		report exited 1
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
fetched=1
jit="$(cat "$body")"
rm -f "$body"
if [ -z "$jit" ]; then
	log "empty JIT config; powering off"
	exit 0
fi

if ! cd "$RUNNER_DIR" 2>/dev/null || [ ! -x ./run.sh ]; then
	report exited 127 # before the log line, which may fail (see below)
	log "no runnable $RUNNER_DIR/run.sh; powering off"
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
(
	# Nothing may end the reader early: a writer to a FIFO with no reader
	# fails (EPIPE), which can end the runner mid-job. So a line the broken
	# journal stream will not take is dropped, never fatal under set -e.
	trap - EXIT TERM INT HUP
	while IFS= read -r line || [ -n "$line" ]; do
		printf '%s\n' "$line" 2>/dev/null || :
		case "$line" in
		*"Listening for Jobs"*) report listening ;;
		*"Running job:"*) report job ;;
		esac
	done <"$fifo"
) &
# Ephemeral JIT runner: registers, runs one job, then auto-deregisters.
rc=0
./run.sh --jitconfig "$jit" >"$fifo" 2>&1 || rc=$?
rm -f "$fifo"
report exited "$rc"
[ "$rc" -eq 0 ] || log "runner exited with status $rc"
