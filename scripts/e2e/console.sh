#!/usr/bin/env bash
# console.sh — print the tail of an instance's serial console, decoded.
#
# Usage: scripts/e2e/console.sh [INSTANCE] [-n LINES]
#
# INSTANCE defaults to FR_E2E_CONTROL_INSTANCE; LINES defaults to 100 (0 prints
# everything). This is where a runner that never registered shows why: no
# network on boot, a failed JIT fetch, the boot unit's own log lines. The
# console also survives the instance: a terminated runner's stays readable for
# a while, and needs no SSM agent.
#
# Exit 1 if EC2 has no console output for the instance yet (it lags boot by up
# to a few minutes on some instance types), 2 on bad usage or configuration.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

instance="" lines=100
while [ $# -gt 0 ]; do
	case "$1" in
	-n) lines="${2:-}" && shift ;;
	-h | --help)
		sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	-*) e2e_die 2 "unknown option: $1 (see --help)" ;;
	*)
		[ -z "$instance" ] || e2e_die 2 "more than one instance given"
		instance="$1"
		;;
	esac
	shift
done
case "$lines" in '' | *[!0-9]*) e2e_die 2 "-n wants a number of lines, got '$lines'" ;; esac
instance="$(e2e_instance "$instance")"
e2e_aws

# --latest (the live buffer) works on Nitro instances only; fall back to the
# snapshot EC2 takes at boot and shutdown. The CLI decodes the base64 Output.
out="$(aws ec2 get-console-output --instance-id "$instance" --latest --query Output --output text 2>/dev/null)" ||
	out="$(aws ec2 get-console-output --instance-id "$instance" --query Output --output text)" ||
	e2e_die 1 "could not read the console of $instance"
if [ -z "$out" ] || [ "$out" = None ]; then
	e2e_die 1 "no console output for $instance yet; EC2 can take a few minutes to capture it"
fi
if [ "$lines" = 0 ]; then
	printf '%s\n' "$out" | tr -d '\r'
else
	printf '%s\n' "$out" | tr -d '\r' | tail -n "$lines"
fi
