#!/usr/bin/env bash
# build-ec2-ami.sh — build the firerunner EC2 runner AMI and publish its ID.
#
# Launches a build instance from stock Ubuntu 24.04 with images/ec2/provision.sh
# (plus the boot script and unit) as user-data, waits for it to power off,
# snapshots it, publishes the AMI ID to an SSM parameter, and terminates the
# build instance. The build instance is terminated on every exit path.
#
# Usage:
#   images/build-ec2-ami.sh --subnet ID --security-group ID --ssm-param NAME [options]
#   images/build-ec2-ami.sh --render [--runner-version V] [--arch x64|arm64]
#
# Options:
#   --region R            AWS region (default: AWS_REGION / profile)
#   --subnet ID           subnet for the build instance; it needs egress to
#                         Ubuntu's mirrors and github.com
#   --security-group ID   security group for the build instance (no inbound needed)
#   --ssm-param NAME      SSM parameter that receives the AMI ID (a tier's "ami")
#   --instance-type T     build instance type (default c7a.large; arm64: c7g.large)
#   --arch A              x64 (default) or arm64
#   --runner-version V    actions/runner version (default: latest release)
#   --volume-gib N        root volume size (default 20)
#   --tag K=V             extra tag on the instance, AMI and snapshot (repeatable)
#   --timeout SECONDS     give up waiting for the build (default 1200)
#   --render              print the build user-data and exit; touches no AWS
#
# Output: progress on stderr; on success one JSON line on stdout:
#   {"ami":"ami-…","ssm_param":"…","runner_version":"…","arch":"…"}
#
# GitHub accepts runner versions for about 30 days after their release, so
# rebuild at least monthly; tiers that name the SSM parameter pick up the new
# image within five minutes with no config change.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

region="" subnet="" sg="" param="" itype="" arch="x64" runner_version="" volume=20
timeout_s=1200 render=0
tags=()

die() {
	echo "build-ec2-ami: $*" >&2
	exit 2
}
log() { echo "build-ec2-ami: $*" >&2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--region) region="${2:?}"; shift ;;
	--subnet) subnet="${2:?}"; shift ;;
	--security-group) sg="${2:?}"; shift ;;
	--ssm-param) param="${2:?}"; shift ;;
	--instance-type) itype="${2:?}"; shift ;;
	--arch) arch="${2:?}"; shift ;;
	--runner-version) runner_version="${2:?}"; shift ;;
	--volume-gib) volume="${2:?}"; shift ;;
	--tag) tags+=("${2:?}"); shift ;;
	--timeout) timeout_s="${2:?}"; shift ;;
	--render) render=1 ;;
	-h | --help)
		sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
	shift
done

case "$arch" in
x64) ubuntu_arch=amd64 default_type=c7a.large ;;
arm64) ubuntu_arch=arm64 default_type=c7g.large ;;
*) die "--arch must be x64 or arm64" ;;
esac
itype="${itype:-$default_type}"
[[ "$volume" =~ ^[0-9]+$ ]] || die "--volume-gib must be a number"
[[ "$timeout_s" =~ ^[0-9]+$ ]] || die "--timeout must be a number of seconds"
for t in "${tags[@]+"${tags[@]}"}"; do
	[[ "$t" == ?*=* ]] || die "--tag wants KEY=VALUE, got '$t'"
	# The CLI shorthand {Key=K,Value=V} cannot carry these characters.
	[[ "$t" != *[,\[\]\{\}]* ]] || die "--tag '$t' contains one of , [ ] { }, which the AWS CLI shorthand cannot carry"
done

if [ -z "$runner_version" ]; then
	runner_version="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r .tag_name)"
	runner_version="${runner_version#v}"
fi
[[ "$runner_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad runner version '$runner_version'"

# render prints the build instance's user-data: the settings, a function that
# installs the boot script and unit, then provision.sh's body.
render_userdata() {
	echo '#!/bin/bash'
	printf 'RUNNER_VERSION=%q\nRUNNER_ARCH=%q\nexport RUNNER_VERSION RUNNER_ARCH\n' "$runner_version" "$arch"
	echo 'install_boot_unit() {'
	echo "cat >/usr/local/bin/firerunner-ec2-run.sh <<'FIRERUNNER_RUN_SH'"
	cat "$HERE/assets/firerunner-ec2-run.sh"
	echo 'FIRERUNNER_RUN_SH'
	echo 'chmod 0755 /usr/local/bin/firerunner-ec2-run.sh'
	echo "cat >/etc/systemd/system/firerunner-ec2-runner.service <<'FIRERUNNER_UNIT'"
	cat "$HERE/assets/firerunner-ec2-runner.service"
	echo 'FIRERUNNER_UNIT'
	echo 'systemctl enable firerunner-ec2-runner.service'
	echo '}'
	sed '1d' "$HERE/ec2/provision.sh"
}

if [ "$render" = 1 ]; then
	render_userdata
	exit 0
fi

[ -n "$subnet" ] || die "--subnet is required"
[ -n "$sg" ] || die "--security-group is required"
[ -n "$param" ] || die "--ssm-param is required"
aws_args=()
[ -z "$region" ] || aws_args+=(--region "$region")
aws() { command aws "${aws_args[@]+"${aws_args[@]}"}" "$@"; }

work="$(mktemp -d)"
instance=""
cleanup() {
	if [ -n "$instance" ]; then
		log "terminating build instance $instance"
		aws ec2 terminate-instances --instance-ids "$instance" >/dev/null || log "could not terminate $instance; terminate it by hand"
	fi
	rm -rf "$work"
}
trap cleanup EXIT

render_userdata >"$work/user-data.sh"
size=$(wc -c <"$work/user-data.sh")
[ "$size" -le 16384 ] || die "user-data is $size bytes; EC2 allows 16384"

base_ami="$(aws ssm get-parameter \
	--name "/aws/service/canonical/ubuntu/server/24.04/stable/current/${ubuntu_arch}/hvm/ebs-gp3/ami-id" \
	--query Parameter.Value --output text)"
log "base image $base_ami, runner $runner_version ($arch), building on $itype"

name="firerunner-runner-${arch}-$(date -u +%Y%m%d-%H%M%S)"
tag_list="{Key=Name,Value=$name},{Key=firerunner:build,Value=$runner_version}"
for t in "${tags[@]+"${tags[@]}"}"; do
	tag_list+=",{Key=${t%%=*},Value=${t#*=}}"
done

instance="$(aws ec2 run-instances \
	--image-id "$base_ami" --instance-type "$itype" --count 1 \
	--subnet-id "$subnet" --security-group-ids "$sg" \
	--user-data "file://$work/user-data.sh" \
	--metadata-options HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled \
	--instance-initiated-shutdown-behavior stop \
	--block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=$volume,VolumeType=gp3,DeleteOnTermination=true}" \
	--tag-specifications "ResourceType=instance,Tags=[$tag_list]" "ResourceType=volume,Tags=[$tag_list]" \
	--query 'Instances[0].InstanceId' --output text)"
log "build instance $instance launched; waiting for it to power off (up to ${timeout_s}s)"

deadline=$(($(date +%s) + timeout_s))
past_deadline() { [ "$(date +%s)" -ge "$deadline" ]; }
# A just-launched instance can be briefly unknown to describe calls.
aws ec2 wait instance-exists --instance-ids "$instance"
while :; do
	state="$(aws ec2 describe-instances --instance-ids "$instance" \
		--query 'Reservations[0].Instances[0].State.Name' --output text)" || state=unknown
	case "$state" in
	stopped) break ;;
	terminated | shutting-down) die "build instance $instance ended as $state" ;;
	esac
	past_deadline && die "build did not finish within ${timeout_s}s (state $state); its console: aws ec2 get-console-output --instance-id $instance"
	sleep 15
done

# provision.sh powers off on failure too: only its "finished" mark on the
# console says the image is good. Console output can lag the stop.
while :; do
	console="$(aws ec2 get-console-output --instance-id "$instance" --latest --query Output --output text 2>/dev/null || true)"
	if grep -q "FIRERUNNER-BUILD .* FAILED" <<<"$console"; then
		die "provisioning failed: $(grep -o "FIRERUNNER-BUILD .* FAILED.*" <<<"$console" | head -n 1)"
	fi
	grep -q "FIRERUNNER-BUILD .* finished" <<<"$console" && break
	past_deadline && die "build instance stopped without a 'finished' mark on its console; not imaging it"
	sleep 10
done

ami="$(aws ec2 create-image --instance-id "$instance" --name "$name" \
	--description "firerunner EC2 runner, actions/runner $runner_version ($arch)" \
	--tag-specifications "ResourceType=image,Tags=[$tag_list]" "ResourceType=snapshot,Tags=[$tag_list]" \
	--query ImageId --output text)"
log "creating $ami"
# Poll against our own deadline: the CLI waiter gives up after ~10 minutes,
# which a first snapshot can exceed.
while :; do
	img_state="$(aws ec2 describe-images --image-ids "$ami" --query 'Images[0].State' --output text)" || img_state=unknown
	case "$img_state" in
	available) break ;;
	failed | invalid | error) die "image $ami ended as $img_state" ;;
	esac
	past_deadline && die "image $ami still $img_state after ${timeout_s}s; it is not published"
	sleep 15
done
aws ssm put-parameter --name "$param" --type String --value "$ami" --overwrite >/dev/null
log "published $ami to $param"

printf '{"ami":"%s","ssm_param":"%s","runner_version":"%s","arch":"%s"}\n' "$ami" "$param" "$runner_version" "$arch"
