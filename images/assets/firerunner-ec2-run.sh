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
set -eu

IMDS="${FIRERUNNER_IMDS:-169.254.169.254}"
RUNNER_DIR="${FIRERUNNER_RUNNER_DIR:-/opt/runner}"
POWEROFF="${FIRERUNNER_POWEROFF:-systemctl poweroff}"
RETRY_SLEEP="${FIRERUNNER_RETRY_SLEEP:-0.2}"
# Give up on a config that never arrives; firerunner revokes it long before.
JIT_WAIT="${FIRERUNNER_JIT_WAIT:-600}"

log() { echo "firerunner: $*" >&2; }

finish() {
	sync
	# shellcheck disable=SC2086 # POWEROFF is a command line.
	$POWEROFF
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
	# A warm-up or image-build boot: nothing to run.
	log "no FIRERUNNER_JIT_URL in user-data; powering off"
	exit 0
fi

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

cd "$RUNNER_DIR"
# The instance is single-use and thrown away after the job.
export RUNNER_ALLOW_RUNASROOT=1
export HOME=/root
export USER=root
# Ephemeral JIT runner: registers, runs one job, then auto-deregisters.
./run.sh --jitconfig "$jit" || log "runner exited non-zero"
