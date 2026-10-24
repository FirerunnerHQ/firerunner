#!/usr/bin/env bash
# control-vm.sh — this session's own control VM, as a stack made from
# deploy/cloudformation/control.yaml.
#
# Usage:
#   scripts/e2e/control-vm.sh up [--version V] [--tiers FILE]
#   scripts/e2e/control-vm.sh status
#   scripts/e2e/control-vm.sh install FILE
#   scripts/e2e/control-vm.sh down
#   scripts/e2e/control-vm.sh list
#
# Every session gets its own stack, FR_E2E_STACK-FR_OWNER, with Owner set to
# FR_OWNER (see env.sh for its default). The template scopes the VM's role to
# that owner, so a session's firerunner cannot terminate, start or tag another
# owner's instances. The stack copies the networking and instance parameters
# of the shared stack FR_E2E_STACK, and uses its private zone with the JIT
# name <FR_OWNER>.<zone>.
#
#   up       writes the session's SSM parameters under
#            <shared prefix>/sessions/<FR_OWNER>/: env and the app key (or
#            token) copied from the shared stack's, the tier catalog (the
#            shared one, or FILE) with every tier name and label suffixed
#            -FR_E2E_TIER_SUFFIX, any runner AMI parameter it names, and the
#            release (V, else FR_E2E_VERSION, else the shared stack's) with its
#            pinned checksums. Then it creates the stack and waits until
#            firerunner is active on the VM. If the stack exists, it only
#            waits. Runs on the session's tiers use `runs-on: <tier name>`
#            from the JSON's tiers.
#   status   the stack, its VM, whether firerunner is active there, and the
#            session's tier names.
#   install  replaces the VM's firerunner binary with FILE (a linux build for
#            the VM's architecture), through FR_E2E_BUCKET and a presigned
#            URL, and restarts it. A VM the group replaces boots the release
#            again: install again.
#   down     deletes the stack, then cleans up the owner's runners
#            (inventory.sh --cleanup --yes), its JIT name and its SSM
#            parameters. It refuses a stack control-vm.sh did not make for
#            FR_OWNER.
#   list     the live control VMs of every session, with their owners.
#
# up and down wait at most FR_E2E_STACK_DEADLINE seconds (1200). The last line
# on stdout is one JSON object (list: an array). Exit 0 done, 1 failed (the
# log says where), 2 usage or configuration, 124 the deadline passed.
# Needs AWS_REGION, FR_E2E_STACK, jq and curl.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"
TEMPLATE="$HERE/../../deploy/cloudformation/control.yaml"

usage() {
	sed -n '2,44p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}
cmd="${1:-}"
case "$cmd" in
up | status | install | down | list) shift ;;
-h | --help) usage ;;
'') usage 2 >&2 ;;
*) e2e_log "unknown command: $cmd" && usage 2 >&2 ;;
esac
version="" tiers_file="" binary=""
while [ $# -gt 0 ]; do
	case "$cmd $1" in
	"up --version") version="${2:-}" && shift ;;
	"up --tiers") tiers_file="${2:-}" && shift ;;
	*" -h" | *" --help") usage ;;
	"install "[!-]*)
		[ -z "$binary" ] || e2e_die 2 "install takes one FILE"
		binary="$1"
		;;
	*) e2e_die 2 "unknown argument for $cmd: $1 (see --help)" ;;
	esac
	shift
done

e2e_aws
e2e_need_cmd jq
if [ "$cmd" = list ]; then
	e2e_control_vms || e2e_die 1 "could not list the control VMs"
	exit 0
fi
e2e_require FR_E2E_STACK
e2e_check_name FR_E2E_STACK "$FR_E2E_STACK" any-case
owner="$(e2e_owner)"
base="$FR_E2E_STACK"
stack="$base-$owner"
asg="$stack-control"
deadline="$(e2e_seconds FR_E2E_STACK_DEADLINE 1200)"
poll="$(e2e_seconds FR_E2E_POLL 5)"
give_up=$(($(e2e_now) + deadline))

work="$(mktemp -d)"
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT

# stack_json NAME: the stack's description, or nothing if it does not exist.
stack_json() {
	local out
	if out="$(aws cloudformation describe-stacks --stack-name "$1" --output json 2>"$work/err")"; then
		jq -c '.Stacks[0]' <<<"$out"
	elif ! grep -q "does not exist" "$work/err"; then
		e2e_die 1 "describe-stacks $1: $(cat "$work/err")"
	fi
}
# output STACK_JSON KEY, param STACK_JSON KEY, tag STACK_JSON KEY
output() { jq -r --arg k "$2" '[.Outputs // [] | .[] | select(.OutputKey == $k)][0].OutputValue // empty' <<<"$1"; }
param() { jq -r --arg k "$2" '[.Parameters // [] | .[] | select(.ParameterKey == $k)][0].ParameterValue // empty' <<<"$1"; }
tag() { jq -r --arg k "$2" '[.Tags // [] | .[] | select(.Key == $k)][0].Value // empty' <<<"$1"; }

# ssm_get NAME: the parameter's value, decrypted; nothing if it does not exist.
ssm_get() {
	local out
	if out="$(aws ssm get-parameter --name "$1" --with-decryption --query Parameter.Value --output text 2>"$work/err")"; then
		printf '%s\n' "$out"
	elif ! grep -q ParameterNotFound "$work/err"; then
		e2e_die 1 "get-parameter $1: $(cat "$work/err")"
	fi
}
# ssm_put NAME TYPE VALUE. The value goes through a private file, never argv.
ssm_put() {
	jq -n --arg n "$1" --arg t "$2" --arg v "$3" '{Name: $n, Type: $t, Value: $v, Overwrite: true}' >"$work/put.json"
	aws ssm put-parameter --cli-input-json "file://$work/put.json" >/dev/null || e2e_die 1 "put-parameter $1 failed"
	rm -f "$work/put.json"
}

# instance: the group's in-service VM, or nothing.
instance() {
	local i
	# shellcheck disable=SC2016 # a JMESPath literal
	i="$(aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$asg" \
		--query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId | [0]' --output text 2>/dev/null)" || i=""
	case "$i" in i-*) echo "$i" ;; esac
}

# probe INSTANCE: "<is-active state> <version>" of firerunner on the VM.
probe() {
	local out
	out="$("$HERE/remote-exec.sh" "$1" --deadline 30 -- \
		'systemctl is-active firerunner || true; /usr/local/bin/firerunner version 2>/dev/null || true' 2>"$work/probe-err")" || true
	printf '%s\n' "$out" | tr '\n' ' ' | sed 's/ *$//'
}

past_deadline() { [ "$(e2e_now)" -ge "$give_up" ]; }

# report STACK_STATUS INSTANCE FIRERUNNER: the session's state as JSON.
report() {
	local tiers prefix
	prefix="$(param "$s" ParameterPrefix)"
	tiers="[]"
	if [ -n "$prefix" ]; then
		tiers="$(ssm_get "$prefix/control/tiers" 2>/dev/null)" || tiers=""
		tiers="$(jq -c '[.[]?.name]' <<<"${tiers:-[]}" 2>/dev/null)" || tiers="[]"
	fi
	jq -cn --arg o "$owner" --arg st "$stack" --arg ss "$1" --arg i "$2" --arg fr "$3" \
		--arg p "$prefix" --arg jit "$(output "$s" JitHost)" --argjson tiers "$tiers" '{
		owner: $o, stack: $st, stack_status: $ss,
		instance: (if $i == "" then null else $i end),
		firerunner: (if $fr == "" then null else ($fr | split(" ")[0]) end),
		version: (if ($fr | split(" ") | length) > 2 then ($fr | split(" ")[2]) else null end),
		jit_host: (if $jit == "" then null else $jit end),
		prefix: (if $p == "" then null else $p end), tiers: $tiers
	}'
}

# mine_or_die: refuse the stack in $s unless control-vm.sh made it for this
# owner. Another session whose owner collides must never wait on, install
# into or delete this one.
mine_or_die() {
	[ "$(tag "$s" firerunner:e2e-session)" = "$owner" ] ||
		e2e_die 1 "refusing: stack $stack was not made by control-vm.sh for $owner (no firerunner:e2e-session=$owner tag); export another FR_OWNER"
}

# not_shared_owner STACK_JSON: refuse when FR_OWNER is the shared stack's own
# owner, whose pool and leftovers belong to the shared install.
not_shared_owner() {
	[ "$(param "$1" Owner)" != "$owner" ] ||
		e2e_die 2 "FR_OWNER=$owner is the shared stack's own owner; choose one for this session"
}

# --- up ----------------------------------------------------------------------

seed_and_create() {
	local b base_prefix prefix zone_id jit zone repo base_version sums env key token tiers suffix keys a
	b="$(stack_json "$base")"
	[ -n "$b" ] || e2e_die 2 "FR_E2E_STACK=$base: no such stack"
	not_shared_owner "$b"
	base_prefix="$(output "$b" ParameterPrefix)"
	zone_id="$(output "$b" HostedZoneId)"
	jit="$(output "$b" JitHost)"
	zone="${jit#*.}"
	repo="$(param "$b" ReleaseRepo)"
	[ -n "$base_prefix" ] && [ -n "$zone_id" ] && [ -n "$jit" ] ||
		e2e_die 2 "$base lacks the ParameterPrefix, HostedZoneId or JitHost output: is it a deploy/cloudformation/control.yaml stack?"
	prefix="$base_prefix/sessions/$owner"

	base_version="$(ssm_get "$base_prefix/control/version")"
	version="${version:-${FR_E2E_VERSION:-$base_version}}"
	[ -n "$version" ] || e2e_die 2 "no release to run: pass --version or set FR_E2E_VERSION"
	if [ "$version" = "$base_version" ]; then
		sums="$(ssm_get "$base_prefix/control/sha256sums")"
	else
		e2e_need_cmd curl
		sums="$(curl -fsSL --max-time 60 "https://github.com/$repo/releases/download/$version/SHA256SUMS")" ||
			e2e_die 2 "no release $version in $repo"
	fi
	grep -qE '  firerunner_linux_(amd64|arm64)$' <<<"$sums" || e2e_die 1 "the checksums of $version list no firerunner binary"

	env="$(ssm_get "$base_prefix/control/env")"
	[ -n "$env" ] || e2e_die 1 "$base_prefix/control/env is missing"
	key="$(ssm_get "$base_prefix/control/app-private-key")"
	token=""
	[ -n "$key" ] || token="$(ssm_get "$base_prefix/control/token")"
	[ -n "$key$token" ] || e2e_die 1 "neither $base_prefix/control/app-private-key nor token exists"
	if [ -n "$tiers_file" ]; then
		[ -f "$tiers_file" ] || e2e_die 2 "--tiers $tiers_file: no such file"
		tiers="$(cat "$tiers_file")"
	else
		tiers="$(ssm_get "$base_prefix/control/tiers")"
	fi
	jq -e 'type == "array" and length > 0 and all(.[]; .name | type == "string")' <<<"$tiers" >/dev/null 2>&1 ||
		e2e_die 2 "the tier catalog is not a JSON array of named tiers"
	suffix="$(e2e_tier_suffix)"

	# AMI parameters under the shared prefix move under the session's, the
	# only place its role may read.
	while IFS= read -r a; do
		[ -n "$a" ] || continue
		v="$(ssm_get "$a")"
		[ -n "$v" ] || e2e_die 1 "the catalog names $a, which does not exist"
		ssm_put "$prefix/${a#"$base_prefix"/}" String "$v"
	done < <(jq -r --arg p "$base_prefix/" '[.[] | .ec2.ami // empty | select(startswith($p))] | unique[]' <<<"$tiers")
	tiers="$(jq -c --arg sfx "-$suffix" --arg p "$base_prefix/" --arg s "$prefix/" 'map(
		.name += $sfx
		| if .labels then .labels |= map(. + $sfx) else . end
		| if ((.ec2.ami // "") | startswith($p)) then .ec2.ami = $s + (.ec2.ami | ltrimstr($p)) else . end)' <<<"$tiers")"

	ssm_put "$prefix/control/env" String "$env"
	ssm_put "$prefix/control/tiers" String "$tiers"
	if [ -n "$key" ]; then
		ssm_put "$prefix/control/app-private-key" SecureString "$key"
	else
		ssm_put "$prefix/control/token" SecureString "$token"
	fi
	ssm_put "$prefix/control/sha256sums" String "$sums"
	ssm_put "$prefix/control/version" String "$version"
	e2e_log "parameters under $prefix: $version, tiers $(jq -r '[.[].name] | join(" ")' <<<"$tiers")"

	keys="$(aws cloudformation get-template-summary --template-body "file://$TEMPLATE" \
		--query 'Parameters[].ParameterKey' --output json)" || e2e_die 1 "get-template-summary failed"
	jq -c --argjson keys "$keys" --arg o "$owner" --arg p "$prefix" --arg z "$zone_id" --arg zn "$zone" '
		[.Parameters[] | select(.ParameterKey | IN("Owner", "ParameterPrefix", "HostedZoneId", "ZoneName", "JitName") | not)
			| {ParameterKey, ParameterValue}]
		+ [{ParameterKey: "Owner", ParameterValue: $o}, {ParameterKey: "ParameterPrefix", ParameterValue: $p},
			{ParameterKey: "HostedZoneId", ParameterValue: $z}, {ParameterKey: "ZoneName", ParameterValue: $zn},
			{ParameterKey: "JitName", ParameterValue: $o}]
		| map(select(.ParameterKey | IN($keys[])))' <<<"$b" >"$work/params.json"
	aws cloudformation create-stack --stack-name "$stack" --template-body "file://$TEMPLATE" \
		--capabilities CAPABILITY_IAM --parameters "file://$work/params.json" \
		--tags "Key=firerunner:e2e-session,Value=$owner" >/dev/null || e2e_die 1 "create-stack $stack failed"
	e2e_log "creating stack $stack (owner $owner)"
}

cmd_up() {
	local st i fr=""
	s="$(stack_json "$stack")"
	if [ -n "$s" ]; then
		mine_or_die
		e2e_log "stack $stack exists ($(jq -r .StackStatus <<<"$s")); waiting for it"
	else
		seed_and_create
	fi
	while :; do
		s="$(stack_json "$stack")"
		st=gone
		[ -z "$s" ] || st="$(jq -r .StackStatus <<<"$s")"
		case "$st" in
		CREATE_COMPLETE | UPDATE_COMPLETE) break ;;
		CREATE_IN_PROGRESS | UPDATE_IN_PROGRESS | UPDATE_COMPLETE_CLEANUP_IN_PROGRESS) ;;
		*)
			aws cloudformation describe-stack-events --stack-name "$stack" --output text \
				--query "StackEvents[?ResourceStatus=='CREATE_FAILED'].[LogicalResourceId,ResourceStatusReason]" 2>/dev/null |
				head -n 5 | while IFS= read -r line; do e2e_log "  $line"; done
			e2e_log "stack $stack is $st; remove it with: scripts/e2e/control-vm.sh down"
			report "$st" "" ""
			exit 1
			;;
		esac
		if past_deadline; then
			e2e_log "stack $stack still $st after ${deadline}s"
			report "$st" "" ""
			exit 124
		fi
		sleep "$poll"
	done
	e2e_log "stack $stack is $st; waiting for firerunner on its VM"
	while :; do
		i="$(instance)"
		if [ -n "$i" ]; then
			fr="$(probe "$i")"
			case "$fr" in active*)
				e2e_log "firerunner is active on $i: ${fr#active }"
				report "$st" "$i" "$fr"
				exit 0
				;;
			esac
		fi
		if past_deadline; then
			e2e_log "firerunner not active after ${deadline}s (${fr:-no in-service VM})"
			[ -z "$i" ] || e2e_log "why the VM did not boot: scripts/e2e/console.sh $i -n 200"
			e2e_log "replacements: aws autoscaling describe-scaling-activities --auto-scaling-group-name $asg"
			report "$st" "$i" "$fr"
			exit 124
		fi
		sleep "$poll"
	done
}

# --- status, install ---------------------------------------------------------

cmd_status() {
	local i="" fr=""
	s="$(stack_json "$stack")"
	if [ -z "$s" ]; then
		report absent "" ""
		exit 0
	fi
	i="$(instance)"
	[ -z "$i" ] || fr="$(probe "$i")"
	report "$(jq -r .StackStatus <<<"$s")" "$i" "${fr:-unknown}"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

cmd_install() {
	local i obj url sum out rc=0
	[ -n "$binary" ] || e2e_die 2 "install needs a FILE (a linux firerunner build)"
	[ -f "$binary" ] || e2e_die 2 "$binary: no such file"
	e2e_require FR_E2E_BUCKET
	s="$(stack_json "$stack")"
	[ -n "$s" ] || e2e_die 1 "no stack $stack: run scripts/e2e/control-vm.sh up first"
	mine_or_die
	i="$(instance)"
	[ -n "$i" ] || e2e_die 1 "stack $stack has no in-service VM yet"
	obj="s3://$FR_E2E_BUCKET/e2e/$owner/firerunner"
	sum="$(sha256 "$binary")"
	aws s3 cp "$binary" "$obj" --only-show-errors >&2 || e2e_die 1 "upload to $obj failed"
	url="$(aws s3 presign "$obj" --expires-in 900)" || e2e_die 1 "presign $obj failed"
	case "$url" in *"'"*) e2e_die 1 "unexpected quote in the presigned URL" ;; esac
	out="$("$HERE/remote-exec.sh" "$i" --deadline 180 -- "set -eu
curl -fsSL --retry 3 -o /tmp/firerunner.new '$url'
echo '$sum  /tmp/firerunner.new' | sha256sum -c - >/dev/null
chmod 0755 /tmp/firerunner.new
/tmp/firerunner.new version >/dev/null
install -m 0755 /tmp/firerunner.new /usr/local/bin/firerunner
rm -f /tmp/firerunner.new
systemctl restart firerunner
sleep 10
systemctl is-active firerunner || true
/usr/local/bin/firerunner version")" || rc=$?
	[ "$rc" = 0 ] || e2e_die "$rc" "installing on $i failed (exit $rc)"
	fr="$(printf '%s\n' "$out" | tr '\n' ' ' | sed 's/ *$//')"
	report "$(jq -r .StackStatus <<<"$s")" "$i" "$fr"
	case "$fr" in active*) e2e_log "firerunner ${fr#active } is active on $i" ;; *)
		e2e_log "firerunner is not active on $i after the install ($fr): scripts/e2e/remote-exec.sh $i -- journalctl -u firerunner -n 50 --no-pager"
		exit 1
		;;
	esac
}

# --- down --------------------------------------------------------------------

cmd_down() {
	local prefix="" zone_id="" jit="" st b cleanup rc=0 names n=0 rrs dns=false
	b="$(stack_json "$base")"
	[ -z "$b" ] || not_shared_owner "$b"
	s="$(stack_json "$stack")"
	if [ -n "$s" ]; then
		mine_or_die
		prefix="$(param "$s" ParameterPrefix)"
		zone_id="$(param "$s" HostedZoneId)"
		jit="$(output "$s" JitHost)"
		aws cloudformation delete-stack --stack-name "$stack" || e2e_die 1 "delete-stack $stack failed"
		e2e_log "deleting stack $stack"
		while :; do
			s="$(stack_json "$stack")"
			[ -n "$s" ] || break
			st="$(jq -r .StackStatus <<<"$s")"
			case "$st" in
			DELETE_COMPLETE) break ;;
			DELETE_FAILED) e2e_die 1 "stack $stack: DELETE_FAILED; see aws cloudformation describe-stack-events --stack-name $stack" ;;
			esac
			past_deadline && e2e_die 124 "stack $stack still $st after ${deadline}s"
			sleep "$poll"
		done
		e2e_log "stack $stack deleted"
	else
		e2e_log "no stack $stack; cleaning up what $owner may have left"
		[ -z "$b" ] || prefix="$(output "$b" ParameterPrefix)/sessions/$owner"
		[ -z "$b" ] || zone_id="$(output "$b" HostedZoneId)"
		[ -z "$b" ] || { jit="$(output "$b" JitHost)" && jit="$owner.${jit#*.}"; }
	fi

	# The VM is gone, so nothing replaces what this removes.
	cleanup="$(FR_OWNER="$owner" "$HERE/inventory.sh" --cleanup --yes | tail -n 1)" || rc=1
	jq -e . <<<"$cleanup" >/dev/null 2>&1 || cleanup=null

	if [ -n "$zone_id" ] && [ -n "$jit" ]; then
		rrs="$(aws route53 list-resource-record-sets --hosted-zone-id "$zone_id" --output json \
			--query "ResourceRecordSets[?Name=='$jit.' && Type=='A']" 2>/dev/null)" || rrs='[]'
		if [ "$(jq 'length' <<<"$rrs" 2>/dev/null || echo 0)" -gt 0 ]; then
			if aws route53 change-resource-record-sets --hosted-zone-id "$zone_id" --change-batch \
				"$(jq -c '{Changes: [.[] | {Action: "DELETE", ResourceRecordSet: .}]}' <<<"$rrs")" >/dev/null; then
				dns=true
				e2e_log "deleted the JIT name $jit"
			else
				e2e_log "could not delete the JIT name $jit"
				rc=1
			fi
		fi
	fi

	case "$prefix" in
	*/sessions/"$owner")
		names="$(aws ssm get-parameters-by-path --path "$prefix" --recursive \
			--query 'Parameters[].Name' --output json)" || {
			names='[]'
			rc=1
		}
		while [ "$(jq length <<<"$names")" -gt 0 ]; do
			# shellcheck disable=SC2046 # one word per name
			aws ssm delete-parameters --names $(jq -r '.[:10][]' <<<"$names") >/dev/null || rc=1
			n=$((n + $(jq '.[:10] | length' <<<"$names")))
			names="$(jq -c '.[10:]' <<<"$names")"
		done
		[ "$n" = 0 ] || e2e_log "deleted $n parameter(s) under $prefix"
		;;
	'') ;;
	*) e2e_log "not deleting parameters under $prefix: not a session prefix" ;;
	esac

	jq -cn --arg o "$owner" --arg st "$stack" --argjson c "$cleanup" --argjson n "$n" --argjson dns "$dns" \
		'{owner: $o, stack: $st, stack_status: "absent", cleanup: $c, parameters_deleted: $n, jit_name_deleted: $dns}'
	exit "$rc"
}

"cmd_$cmd"
