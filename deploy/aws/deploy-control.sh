#!/usr/bin/env bash
# Deploy a firerunner release to a control plane stack made from control.yaml.
#
#   deploy-control.sh STACK VERSION [--no-wait]
#
# Sets <prefix>/control/version, then replaces the control VM with an
# instance refresh: the old VM is terminated first (two would fight over the
# scale set's session), the new one installs VERSION at boot. Waits for the
# refresh and for firerunner to be active on the new VM, unless --no-wait.
# Roll back by deploying the previous version. Needs the AWS CLI with
# credentials and AWS_REGION (or a profile region) for the stack.
#
# Exit codes: 0 deployed, 1 failed (the message says where), 2 usage.
set -euo pipefail

usage() { echo "usage: deploy-control.sh STACK VERSION [--no-wait]" >&2; exit 2; }
[ $# -ge 2 ] || usage
stack="$1" version="$2" wait=1
[ "${3:-}" = "" ] || [ "$3" = --no-wait ] || usage
[ "${3:-}" = --no-wait ] && wait=0
poll="${DEPLOY_POLL_SECONDS:-15}"
deadline=$(($(date +%s) + ${DEPLOY_TIMEOUT_SECONDS:-1200}))

log() { echo "deploy-control: $*" >&2; }
die() { log "$*"; exit 1; }
trap 'log "failed: $BASH_COMMAND"; exit 1' ERR
output() {
	aws cloudformation describe-stacks --stack-name "$stack" \
		--query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text
}
param_value() {
	aws cloudformation describe-stacks --stack-name "$stack" \
		--query "Stacks[0].Parameters[?ParameterKey=='$1'].ParameterValue" --output text
}
waited() { [ "$(date +%s)" -lt "$deadline" ] || die "timed out: $1"; sleep "$poll"; }

asg=$(output AutoScalingGroup)
prefix=$(output ParameterPrefix)
repo=$(param_value ReleaseRepo)
if [ -z "$asg" ] || [ "$asg" = None ]; then die "stack $stack has no AutoScalingGroup output"; fi

# Refuse a version the VM could not install, before touching anything.
sums="https://github.com/$repo/releases/download/$version/SHA256SUMS"
curl -fsSL -o /dev/null "$sums" || die "no release $version in $repo ($sums)"

previous=$(aws ssm get-parameter --name "$prefix/control/version" --query Parameter.Value --output text 2>/dev/null || echo none)
aws ssm put-parameter --name "$prefix/control/version" --type String --value "$version" --overwrite > /dev/null
log "$prefix/control/version: $previous -> $version"

refresh=$(aws autoscaling start-instance-refresh --auto-scaling-group-name "$asg" \
	--preferences '{"MinHealthyPercentage":0,"MaxHealthyPercentage":100,"InstanceWarmup":60}' \
	--query InstanceRefreshId --output text)
log "instance refresh $refresh started on $asg"
[ "$wait" = 1 ] || exit 0

while :; do
	status=$(aws autoscaling describe-instance-refreshes --auto-scaling-group-name "$asg" \
		--instance-refresh-ids "$refresh" --query 'InstanceRefreshes[0].Status' --output text)
	case "$status" in
	Successful) break ;;
	Failed | Cancelled | RollbackSuccessful | RollbackFailed) die "instance refresh $refresh: $status" ;;
	esac
	waited "instance refresh $refresh still $status"
done

# shellcheck disable=SC2016 # a JMESPath literal
instance=$(aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$asg" \
	--query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId | [0]' --output text)
log "new control VM: $instance"

# InService means EC2 is happy, not that firerunner is: ask systemd.
while :; do
	cmd=$(aws ssm send-command --instance-ids "$instance" --document-name AWS-RunShellScript \
		--parameters 'commands=["systemctl is-active firerunner; /usr/local/bin/firerunner version"]' \
		--query Command.CommandId --output text 2>/dev/null) || cmd=""
	if [ -n "$cmd" ]; then
		sleep 3
		out=$(aws ssm get-command-invocation --command-id "$cmd" --instance-id "$instance" \
			--query StandardOutputContent --output text 2>/dev/null || true)
		if [ "$(head -1 <<< "$out")" = active ]; then
			running=$(sed -n 2p <<< "$out")
			[ "$running" = "firerunner $version" ] || die "$instance runs $running, not $version"
			log "firerunner $version active on $instance"
			exit 0
		fi
	fi
	waited "firerunner not active on $instance"
done
