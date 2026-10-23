#!/usr/bin/env bash
# inventory.sh — list, and clean up, the AWS resources of one owner.
#
# Usage:
#   scripts/e2e/inventory.sh [--owner OWNER]              list
#   scripts/e2e/inventory.sh --cleanup                    show what cleanup would do
#   scripts/e2e/inventory.sh --cleanup --yes              do it
#
# firerunner tags every instance, volume and spot request it creates with
# firerunner:owner=<FR_OWNER>. This lists those of OWNER (default: your own,
# see env.sh) that still exist, as one JSON object on stdout:
#   {owner, instances: [{id, state, type, tier, runner, pool, launched, spot_request}],
#    spot_requests: [{id, state, type, instance}], volumes: [{id, state}],
#    control_vms: [live control VMs of OWNER]}
#
# --cleanup only ever touches your own owner: FR_OWNER, or the one derived
# from this worktree. To clean up a pool you adopted, set FR_OWNER to its
# owner. It refuses while a control VM of that owner is live, since its
# firerunner would replace what is removed: stop it first
# (control-vm.sh down). It is a dry run unless --yes, and in this order:
#   1. cancel the spot requests (a terminated instance whose persistent
#      request is still open is replaced by AWS);
#   2. terminate the instances;
#   3. delete the volumes no instance holds (attached ones go with their
#      instance).
# The last line on stdout is the plan, or what was done, as JSON.
#
# Exit 0 done (or nothing to do), 1 an AWS call failed or cleanup refused,
# 2 usage or configuration.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

owner="" cleanup=0 yes=0
while [ $# -gt 0 ]; do
	case "$1" in
	--owner) owner="${2:-}" && shift ;;
	--cleanup) cleanup=1 ;;
	--yes) yes=1 ;;
	-h | --help)
		sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) e2e_die 2 "unknown argument: $1 (see --help)" ;;
	esac
	shift
done
[ "$yes" = 0 ] || [ "$cleanup" = 1 ] || e2e_die 2 "--yes goes with --cleanup"
mine="$(e2e_owner)"
[ -n "$owner" ] || owner="$mine"
e2e_check_name --owner "$owner"
if [ "$cleanup" = 1 ] && [ "$owner" != "$mine" ]; then
	e2e_die 2 "--cleanup touches only your own owner ($mine), not $owner; to clean up a pool you adopted, set FR_OWNER=$owner"
fi
e2e_aws
e2e_need_cmd jq

tag="Name=tag:firerunner:owner,Values=$owner"
# Each list is filtered again on the tag here, so a filter typo cannot widen it.
# shellcheck disable=SC2016 # a jq program
mine_only='((.Tags // []) | from_entries)["firerunner:owner"] == $o'
instances="$(aws ec2 describe-instances --output json --filters "$tag" \
	Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down |
	jq -c --arg o "$owner" "[.Reservations[].Instances[] | select($mine_only) | ((.Tags // []) | from_entries) as \$t | {
		id: .InstanceId, state: .State.Name, type: .InstanceType, tier: (\$t[\"firerunner:tier\"] // null),
		runner: (\$t[\"firerunner:runner\"] // null), pool: (\$t[\"firerunner:pool\"] // null),
		launched: (.LaunchTime // null), spot_request: (.SpotInstanceRequestId // null)}]")" ||
	e2e_die 1 "describe-instances failed"
spots="$(aws ec2 describe-spot-instance-requests --output json --filters "$tag" Name=state,Values=open,active,disabled |
	jq -c --arg o "$owner" "[.SpotInstanceRequests[] | select($mine_only) |
		{id: .SpotInstanceRequestId, state: .State, type: .Type, instance: (.InstanceId // null)}]")" ||
	e2e_die 1 "describe-spot-instance-requests failed"
volumes="$(aws ec2 describe-volumes --output json --filters "$tag" |
	jq -c --arg o "$owner" "[.Volumes[] | select($mine_only) | {id: .VolumeId, state: .State}]")" ||
	e2e_die 1 "describe-volumes failed"
vms="$(e2e_control_vms)" || e2e_die 1 "could not list the control VMs"
vms="$(jq -c --arg o "$owner" '[.[] | select(.owner == $o)]' <<<"$vms")"

if [ "$cleanup" = 0 ]; then
	jq -cn --arg o "$owner" --argjson i "$instances" --argjson s "$spots" --argjson v "$volumes" --argjson c "$vms" \
		'{owner: $o, instances: $i, spot_requests: $s, volumes: $v, control_vms: $c}'
	exit 0
fi

if [ "$(jq length <<<"$vms")" -gt 0 ]; then
	e2e_log "refused: control VM(s) of $owner are live: $(jq -r '[.[].instance] | join(" ")' <<<"$vms")"
	e2e_log "its firerunner would replace what this removes; stop it first: scripts/e2e/control-vm.sh down"
	exit 1
fi

cancel="$(jq -c '[.[].id]' <<<"$spots")"
terminate="$(jq -c '[.[] | select(.state != "shutting-down") | .id]' <<<"$instances")"
delete="$(jq -c '[.[] | select(.state == "available") | .id]' <<<"$volumes")"
plan() {
	jq -cn --arg o "$owner" --argjson dry "$1" --argjson c "$cancel" --argjson t "$terminate" --argjson d "$delete" \
		'{owner: $o, dry_run: $dry, cancel_spot_requests: $c, terminate_instances: $t, delete_volumes: $d}'
}
if [ "$yes" = 0 ]; then
	e2e_log "dry run; add --yes to do this"
	plan true
	exit 0
fi

ids() { jq -r '.[]' <<<"$1"; }
if [ "$(jq length <<<"$cancel")" -gt 0 ]; then
	# shellcheck disable=SC2046 # one word per ID
	aws ec2 cancel-spot-instance-requests --spot-instance-request-ids $(ids "$cancel") >/dev/null ||
		e2e_die 1 "cancel-spot-instance-requests failed; nothing terminated"
	e2e_log "cancelled spot requests: $(ids "$cancel" | tr '\n' ' ')"
fi
if [ "$(jq length <<<"$terminate")" -gt 0 ]; then
	# shellcheck disable=SC2046 # one word per ID
	aws ec2 terminate-instances --instance-ids $(ids "$terminate") >/dev/null ||
		e2e_die 1 "terminate-instances failed"
	e2e_log "terminating instances: $(ids "$terminate" | tr '\n' ' ')"
fi
rc=0
for v in $(ids "$delete"); do
	aws ec2 delete-volume --volume-id "$v" >/dev/null || {
		e2e_log "delete-volume $v failed"
		rc=1
	}
done
plan false
exit "$rc"
