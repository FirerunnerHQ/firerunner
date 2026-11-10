#!/usr/bin/env bash
# layer-ec2-ami.sh — builds a new EC2 runner AMI from an existing one, replacing
# only firerunner's own files: its binary, boot scripts and units. A full build
# takes about 40 minutes, half of it the first snapshot. This takes a few, because
# EBS stores only the blocks that changed. The OS and tools stay as they were, so
# build-ec2-ami.sh must still run on a schedule for their security updates.
#
# Runs as root on an EC2 instance of the same architecture and region, which needs:
#   ec2:CreateVolume, AttachVolume, DetachVolume, DeleteVolume, DescribeVolumes,
#   CreateSnapshot, DescribeSnapshots, RegisterImage, DescribeImages, CreateTags;
#   ssm:GetParameter and PutParameter on the AMI's parameters.
#
# Usage:
#   images/layer-ec2-ami.sh --base AMI_OR_PARAM --ssm-param NAME --firerunner-version vX.Y.Z
#
# Before it touches AWS, it checks with gh (logged in, e.g. GH_TOKEN) that the
# release's binary carries an attestation from firerunner's release workflow
# for that tag, and installs only a download with the SHA-256 it checked.
# FIRERUNNER_SKIP_ATTESTATION=1 skips that, leaving the release's own SHA256SUMS.
#
# It prints the new image's details as one line of JSON, and publishes the image to
# NAME only once it is available. On any failure nothing is published, and its
# volume is deleted.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMDS="${FIRERUNNER_IMDS:-169.254.169.254}"
ROOT="${FIRERUNNER_LAYER_ROOT:-/mnt/firerunner-layer}"
TRIES="${FIRERUNNER_LAYER_TRIES:-240}"
PAUSE="${FIRERUNNER_LAYER_PAUSE:-5}"

log() { echo "layer-ec2-ami: $*" >&2; }
die() {
	log "$*"
	exit 1
}

base="" param="" version=""
while [ $# -gt 0 ]; do
	case "$1" in
	--base) base="${2:?}" && shift ;;
	--ssm-param) param="${2:?}" && shift ;;
	--firerunner-version) version="${2:?}" && shift ;;
	*) die "unknown argument: $1" ;;
	esac
	shift
done
[ -n "$base" ] || die "--base is required"
[ -n "$param" ] || die "--ssm-param is required"
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "--firerunner-version must be a release tag"

# The binary runs here as root and ships in the image; its release's
# SHA256SUMS proves nothing by itself (build-ec2-ami.sh checks the same way).
# shellcheck source=lib/verify.sh disable=SC1091
. "$HERE/lib/verify.sh"
sha256=""
if [ "${FIRERUNNER_SKIP_ATTESTATION:-}" = 1 ]; then
	log "FIRERUNNER_SKIP_ATTESTATION=1: not checking the build provenance of firerunner $version"
else
	release="$(firerunner_release FirerunnerHQ/firerunner "$version" firerunner_linux_amd64)" ||
		die "cannot check firerunner $version's build provenance (FIRERUNNER_SKIP_ATTESTATION=1 skips this check)"
	sha256="${release#* }"
	log "firerunner $version firerunner_linux_amd64 sha256 $sha256, built by its release workflow"
fi

imds() {
	local token
	token="$(curl -fsS -m 2 -X PUT "http://$IMDS/latest/api/token" -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')"
	curl -fsS -m 2 -H "X-aws-ec2-metadata-token: $token" "http://$IMDS/latest/meta-data/$1"
}

# until_state VALUE CMD... polls CMD's output for VALUE.
until_state() {
	local want="$1" got tries=0
	shift
	while :; do
		got="$("$@")" || got=unknown
		[ "$got" = "$want" ] && return 0
		tries=$((tries + 1))
		[ "$tries" -lt "$TRIES" ] || die "still $got, not $want: $*"
		sleep "$PAUSE"
	done
}

case "$base" in
ami-*) base_ami="$base" ;;
*) base_ami="$(aws ssm get-parameter --name "$base" --query Parameter.Value --output text)" ;;
esac
read -r snapshot boot_mode ena <<<"$(aws ec2 describe-images --image-ids "$base_ami" \
	--query 'Images[0].[BlockDeviceMappings[0].Ebs.SnapshotId,BootMode,EnaSupport]' --output text)"
[ -n "$snapshot" ] && [ "$snapshot" != None ] || die "$base_ami has no root snapshot"

instance="$(imds instance-id)"
zone="$(imds placement/availability-zone)"
volume=""
mounted=""
cleanup() {
	[ -z "$mounted" ] || umount "$ROOT" || log "could not unmount $ROOT"
	if [ -n "$volume" ]; then
		aws ec2 detach-volume --volume-id "$volume" >/dev/null 2>&1 || true
		until_state available aws ec2 describe-volumes --volume-ids "$volume" --query 'Volumes[0].State' --output text ||
			log "$volume did not detach"
		aws ec2 delete-volume --volume-id "$volume" || log "could not delete $volume; delete it by hand"
	fi
}
trap cleanup EXIT

volume="$(aws ec2 create-volume --snapshot-id "$snapshot" --availability-zone "$zone" --volume-type gp3 --query VolumeId --output text)"
log "layering on $base_ami with $volume"
until_state available aws ec2 describe-volumes --volume-ids "$volume" --query 'Volumes[0].State' --output text
aws ec2 attach-volume --volume-id "$volume" --instance-id "$instance" --device /dev/sdf >/dev/null
until_state attached aws ec2 describe-volumes --volume-ids "$volume" --query 'Volumes[0].Attachments[0].State' --output text

device="${FIRERUNNER_LAYER_DEVICE:-/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${volume/-/}}"
root_partition() { lsblk -nrpo NAME,LABEL "$device" 2>/dev/null | awk '$2 == "cloudimg-rootfs" {print $1}'; }
tries=0
until partition="$(root_partition)" && [ -n "$partition" ]; do
	tries=$((tries + 1))
	[ "$tries" -lt "$TRIES" ] || die "no root partition on $device"
	sleep "$PAUSE"
done
mkdir -p "$ROOT"
mount "$partition" "$ROOT"
mounted=1

# shellcheck source=ec2/install-firerunner.sh disable=SC1091
. "$HERE/ec2/install-firerunner.sh"
RUNNER_ARCH=x64 FR_VERSION="$version" FR_SHA256="$sha256" FR_URL="" FIRERUNNER_BIN="$ROOT/usr/local/bin/firerunner" install_firerunner || die "could not install firerunner $version"
assets="$(mktemp -p "$ROOT/tmp")"
"$HERE/build-ec2-ami.sh" --render-assets >"$assets"
chroot "$ROOT" /bin/bash "${assets#"$ROOT"}"
rm -f "$assets"
umount "$ROOT"
mounted=""

aws ec2 detach-volume --volume-id "$volume" >/dev/null
until_state available aws ec2 describe-volumes --volume-ids "$volume" --query 'Volumes[0].State' --output text
name="firerunner-runner-x64-$version-$(date -u +%Y%m%d-%H%M%S)"
new_snapshot="$(aws ec2 create-snapshot --volume-id "$volume" --description "$name" --query SnapshotId --output text)"
until_state completed aws ec2 describe-snapshots --snapshot-ids "$new_snapshot" --query 'Snapshots[0].State' --output text
ami="$(aws ec2 register-image --name "$name" --description "firerunner EC2 runner, $version on $base_ami" \
	--architecture x86_64 --root-device-name /dev/sda1 --virtualization-type hvm --boot-mode "$boot_mode" \
	--imds-support v2.0 "$([ "$ena" = True ] && echo --ena-support || echo --no-ena-support)" \
	--block-device-mappings "DeviceName=/dev/sda1,Ebs={SnapshotId=$new_snapshot,VolumeType=gp3,DeleteOnTermination=true}" \
	--query ImageId --output text)"
until_state available aws ec2 describe-images --image-ids "$ami" --query 'Images[0].State' --output text
aws ssm put-parameter --name "$param" --type String --value "$ami" --overwrite >/dev/null
log "published $ami to $param"
printf '{"ami":"%s","base":"%s","ssm_param":"%s","firerunner":"%s"}\n' "$ami" "$base_ami" "$param" "$version"
