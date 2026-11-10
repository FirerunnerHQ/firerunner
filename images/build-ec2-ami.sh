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
#   --instance-type T     build instance type (default c7a.4xlarge; minimal:
#                         c7a.large; arm64: c7g.large)
#   --arch A              x64 (default) or arm64
#   --runner-version V    actions/runner version (default: latest release)
#   --runner-sha256 H     its tarball's SHA-256 (default: from the release
#                         notes); needs --runner-version
#   --toolset T           standard (default), full or minimal. standard and full
#                         install GitHub's ubuntu-24.04 tools, x64 only, so arm64
#                         defaults to minimal
#   --runner-images-ref R actions/runner-images tag for standard and full
#   --runner-images-commit C  the commit to fetch for it (default: the one
#                         pinned for the default tag; another tag is resolved
#                         to its commit now)
#   --firerunner-version V firerunner release for the git cache's proxy, such as
#                         v0.1.1 (default: the latest release). This machine
#                         checks its attestation with gh (logged in), and the
#                         build instance checks its download against the
#                         SHA-256 it verified; FIRERUNNER_SKIP_ATTESTATION=1
#                         skips that, leaving the release's own SHA256SUMS
#   --firerunner-url URL  take the binary from here instead; needs
#                         --firerunner-sha256. You vouch for this binary:
#                         nothing checks where it was built
#   --firerunner-sha256 H the binary's SHA-256
#   --volume-gib N        root volume size (default 40; full: 80; minimal: 20)
#   --tag K=V             extra tag on the instance, AMI and snapshot (repeatable)
#   --timeout SECONDS     give up waiting for the build (default 3600; minimal:
#                         1200)
#   --render              print the build user-data and exit; touches no AWS
#   --render-assets       print a script that installs firerunner's own files, for
#                         layer-ec2-ami.sh, and exit; touches no AWS
#
# Output: progress on stderr; on success one JSON line on stdout:
#   {"ami":"ami-…","ssm_param":"…","runner_version":"…","runner_sha256":"…","arch":"…"}
#
# GitHub accepts runner versions for about 30 days after their release, so
# rebuild at least monthly; tiers that name the SSM parameter pick up the new
# image within five minutes with no config change.
#
# The build instance checks what it downloads: the runner against its SHA-256,
# runner-images by fetching a commit with git, PowerShell modules against the
# gallery's SHA-512 (ec2/fetch-checked.sh). The default tag's commit is pinned
# below; bump both together, the commit from
# gh api repos/actions/runner-images/commits/TAG --jq .sha
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/verify.sh disable=SC1091
. "$HERE/lib/verify.sh"

region="" subnet="" sg="" param="" itype="" arch="x64" runner_version="" runner_sha256="" volume=""
ri_ref_pinned="ubuntu24/20261101.360" ri_commit_pinned="add4380b0d6685806e9192c33c37359ee0aa4023"
toolset="" ri_ref="$ri_ref_pinned" ri_commit=""
fr_version="" fr_url="" fr_sha256="" fr_repo=FirerunnerHQ/firerunner
ri_skip="configure-apt-mock.sh configure-system.sh cleanup.sh list-dpkg.sh post-build-validation.sh install-android-sdk.sh install-homebrew.sh configure-snap.sh"
timeout_s="" render=0
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
	--runner-sha256) runner_sha256="${2:?}"; shift ;;
	--volume-gib) volume="${2:?}"; shift ;;
	--toolset) toolset="${2:?}"; shift ;;
	--runner-images-ref) ri_ref="${2:?}"; shift ;;
	--runner-images-commit) ri_commit="${2:?}"; shift ;;
	--firerunner-version) fr_version="${2:?}"; shift ;;
	--firerunner-url) fr_url="${2:?}"; shift ;;
	--firerunner-sha256) fr_sha256="${2:?}"; shift ;;
	--tag) tags+=("${2:?}"); shift ;;
	--timeout) timeout_s="${2:?}"; shift ;;
	--render) render=1 ;;
	--render-assets) render=2 ;;
	-h | --help)
		sed -n '2,42p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
	shift
done

case "$arch" in
x64) ubuntu_arch=amd64 default_toolset=standard ;;
arm64) ubuntu_arch=arm64 default_toolset=minimal ;;
*) die "--arch must be x64 or arm64" ;;
esac
toolset="${toolset:-$default_toolset}"
case "$arch/$toolset" in
x64/minimal) default_type=c7a.large default_volume=20 default_timeout=1200 ;;
arm64/minimal) default_type=c7g.large default_volume=20 default_timeout=1200 ;;
*/standard) default_type=c7a.4xlarge default_volume=40 default_timeout=3600 ;;
*/full) default_type=c7a.4xlarge default_volume=80 default_timeout=3600 ;;
*) die "--toolset must be standard, full or minimal" ;;
esac
itype="${itype:-$default_type}"
volume="${volume:-$default_volume}"
timeout_s="${timeout_s:-$default_timeout}"
[[ "$volume" =~ ^[0-9]+$ ]] || die "--volume-gib must be a number"
[ "$toolset" = minimal ] || [ "$arch" = x64 ] || die "--toolset $toolset is x64 only: its installers come from GitHub's x64 toolset"
[ "$toolset" != standard ] || ri_skip="$ri_skip Install-PowerShellModules.ps1 install-haskell.sh install-swift.sh install-julia.sh install-firefox.sh install-microsoft-edge.sh install-google-cloud-cli.sh install-codeql-bundle.sh install-kotlin.sh install-bazel.sh install-pulumi.sh install-packer.sh install-vcpkg.sh install-azure-devops-cli.sh install-copilot-cli.sh install-awf.sh install-pypy.sh"
[[ "$timeout_s" =~ ^[0-9]+$ ]] || die "--timeout must be a number of seconds"
for t in "${tags[@]+"${tags[@]}"}"; do
	[[ "$t" == ?*=* ]] || die "--tag wants KEY=VALUE, got '$t'"
	# The CLI shorthand {Key=K,Value=V} cannot carry these characters.
	[[ "$t" != *[,\[\]\{\}]* ]] || die "--tag '$t' contains one of , [ ] { }, which the AWS CLI shorthand cannot carry"
done

[ -z "$runner_sha256" ] || [ -n "$runner_version" ] || die "--runner-sha256 needs --runner-version"
[ -z "$runner_sha256" ] || [[ "$runner_sha256" =~ ^[0-9a-f]{64}$ ]] || die "bad --runner-sha256 '$runner_sha256'"
[ -z "$runner_version" ] || [[ "$runner_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad runner version '$runner_version'"
if [ "$render" != 2 ]; then
	# A latest runner is resolved once, with the SHA-256 its release notes give
	# the tarball; the build instance checks the download against it.
	if [ -z "$runner_sha256" ]; then
		release="$(runner_release "$runner_version" "linux-$arch")" || die "cannot check the runner download"
		runner_version="${release% *}" runner_sha256="${release#* }"
	fi
	[[ "$runner_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad runner version '$runner_version'"
	log "actions/runner $runner_version linux-$arch sha256 $runner_sha256"
	if [ "$toolset" != minimal ] && [ -z "$ri_commit" ]; then
		if [ "$ri_ref" = "$ri_ref_pinned" ]; then
			ri_commit="$ri_commit_pinned"
		else
			ri_commit="$(resolve_commit actions/runner-images "$ri_ref")" || die "cannot pin runner-images $ri_ref"
			log "resolved runner-images $ri_ref to commit $ri_commit; pass --runner-images-commit $ri_commit to pin it"
		fi
	fi
	[ -z "$ri_commit" ] || [[ "$ri_commit" =~ ^[0-9a-f]{40}$ ]] || die "bad --runner-images-commit '$ri_commit'"
fi
[ -z "$fr_url" ] || [[ "$fr_sha256" =~ ^[0-9a-f]{64}$ ]] || die "--firerunner-url needs --firerunner-sha256"
[ -z "$fr_sha256" ] || [ -n "$fr_url" ] || die "--firerunner-sha256 needs --firerunner-url"
[ -z "$fr_version" ] || [[ "$fr_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "bad firerunner version '$fr_version'"
# A release's binary runs as root on the build instance and ships in the
# image. Its SHA256SUMS comes from the same release, so it proves nothing by
# itself: check here that the release workflow built it from the tag, and
# hand the build instance that tag and the SHA-256 checked.
if [ "$render" != 2 ] && [ -z "$fr_url" ]; then
	if [ "${FIRERUNNER_SKIP_ATTESTATION:-}" = 1 ]; then
		log "FIRERUNNER_SKIP_ATTESTATION=1: not checking the build provenance of firerunner ${fr_version:-latest}"
	else
		release="$(firerunner_release "$fr_repo" "$fr_version" "firerunner_linux_$ubuntu_arch")" ||
			die "cannot check firerunner ${fr_version:-latest}'s build provenance (FIRERUNNER_SKIP_ATTESTATION=1 skips this check)"
		fr_version="${release% *}" fr_sha256="${release#* }"
		log "firerunner $fr_version firerunner_linux_$ubuntu_arch sha256 $fr_sha256, built by $fr_repo's release workflow"
	fi
fi

# strip prints an asset without its comment lines, keeping the first line: the
# comments are for this repository, and EC2 caps compressed user-data at 16 KiB.
strip() {
	sed '1!{/^[[:space:]]*#/d;}' "$1"
}

# render prints the build instance's user-data: the settings, a function that
# installs the boot script and unit, then provision.sh's body.
render_userdata() {
	echo '#!/bin/bash'
	printf 'RUNNER_VERSION=%q\nRUNNER_SHA256=%q\nRUNNER_ARCH=%q\nTOOLSET=%q\nRI_REF=%q\nRI_COMMIT=%q\nRI_SKIP=%q\nFR_VERSION=%q\nFR_URL=%q\nFR_SHA256=%q\nexport RUNNER_VERSION RUNNER_SHA256 RUNNER_ARCH TOOLSET RI_REF RI_COMMIT RI_SKIP FR_VERSION FR_URL FR_SHA256\n' \
		"$runner_version" "$runner_sha256" "$arch" "$toolset" "$ri_ref" "$ri_commit" "$ri_skip" "$fr_version" "$fr_url" "$fr_sha256"
	strip "$HERE/ec2/fetch-checked.sh"
	strip "$HERE/ec2/install-runner-images.sh"
	strip "$HERE/ec2/install-firerunner.sh"
	render_assets
	sed '1d' "$HERE/ec2/provision.sh"
}

# render_assets prints the two functions that install what firerunner adds to an
# image. build-ec2-ami.sh and layer-ec2-ami.sh both use them, so a layer cannot
# drift from a full build.
render_assets() {
	echo 'install_boot_unit() {'
	echo "cat >/usr/local/bin/firerunner-ec2-run.sh <<'FIRERUNNER_RUN_SH'"
	strip "$HERE/assets/firerunner-ec2-run.sh"
	echo 'FIRERUNNER_RUN_SH'
	echo 'chmod 0755 /usr/local/bin/firerunner-ec2-run.sh'
	echo 'install -d /usr/local/libexec'
	echo "cat >/usr/local/libexec/firerunner-ec2-tune <<'FIRERUNNER_TUNE'"
	strip "$HERE/assets/firerunner-ec2-tune.sh"
	echo 'FIRERUNNER_TUNE'
	echo 'chmod 0755 /usr/local/libexec/firerunner-ec2-tune'
	echo "cat >/etc/systemd/system/firerunner-ec2-runner.service <<'FIRERUNNER_UNIT'"
	strip "$HERE/assets/firerunner-ec2-runner.service"
	echo 'FIRERUNNER_UNIT'
	echo 'systemctl enable firerunner-ec2-runner.service'
	echo 'install -d /etc/firerunner'
	echo "cat >/etc/firerunner/syn-reroll.nft <<'FIRERUNNER_SYN'"
	strip "$HERE/assets/firerunner-syn-reroll.nft"
	echo 'FIRERUNNER_SYN'
	local unit
	for unit in firerunner-syn-reroll.service firerunner-syn-reroll-stop.service firerunner-syn-reroll-stop.timer; do
		echo "cat >/etc/systemd/system/$unit <<'FIRERUNNER_SYN_UNIT'"
		strip "$HERE/assets/$unit"
		echo 'FIRERUNNER_SYN_UNIT'
	done
	echo 'systemctl enable firerunner-syn-reroll.service firerunner-syn-reroll-stop.timer'
	echo '}'
	# The dependency cache proxy (#28): inactive until a bundle arrives.
	echo 'install_cache_proxy() {'
	echo 'install -d /usr/local/libexec /etc/firerunner'
	echo "cat >/usr/local/libexec/firerunner-cache-setup <<'FIRERUNNER_CACHE_SETUP'"
	cat "$HERE/assets/firerunner-cache-setup.sh"
	echo 'FIRERUNNER_CACHE_SETUP'
	echo 'chmod 0755 /usr/local/libexec/firerunner-cache-setup'
	echo "cat >/usr/local/libexec/firerunner-git-config <<'FIRERUNNER_GIT_CONFIG'"
	strip "$HERE/assets/firerunner-git-config.sh"
	echo 'FIRERUNNER_GIT_CONFIG'
	echo 'chmod 0755 /usr/local/libexec/firerunner-git-config'
	echo "cat >/etc/systemd/system/firerunner-git-proxy.service <<'FIRERUNNER_GIT_UNIT'"
	strip "$HERE/assets/firerunner-git-proxy.service"
	echo 'FIRERUNNER_GIT_UNIT'
	echo "cat >/etc/firerunner/cache-nginx.conf <<'FIRERUNNER_CACHE_NGINX'"
	cat "$HERE/assets/firerunner-cache-nginx.conf"
	echo 'FIRERUNNER_CACHE_NGINX'
	echo "cat >/tmp/nodewrap.c <<'FIRERUNNER_NODEWRAP'"
	cat "$HERE/assets/nodewrap.c"
	echo 'FIRERUNNER_NODEWRAP'
	echo '}'
}

if [ "$render" = 2 ]; then
	echo '#!/bin/bash'
	echo 'set -euo pipefail'
	render_assets
	echo 'install_boot_unit'
	echo 'install_cache_proxy'
	echo 'rm -f /tmp/nodewrap.c'
	exit 0
fi
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
# cloud-init runs gzip'd user-data as it would the script; EC2 counts the
# compressed bytes against its limit.
gzip -9n <"$work/user-data.sh" >"$work/user-data.gz"
size=$(wc -c <"$work/user-data.gz")
[ "$size" -le 16384 ] || die "user-data is $size bytes compressed; EC2 allows 16384"

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
	--user-data "fileb://$work/user-data.gz" \
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

printf '{"ami":"%s","ssm_param":"%s","runner_version":"%s","runner_sha256":"%s","arch":"%s"}\n' "$ami" "$param" "$runner_version" "$runner_sha256" "$arch"
