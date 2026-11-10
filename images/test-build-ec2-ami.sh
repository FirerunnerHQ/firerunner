#!/usr/bin/env bash
# Tests for build-ec2-ami.sh with a fake `aws` and `curl`: no AWS account or
# network needed. The fake records every aws call and plays the build
# instance through pending -> running -> stopped (or a scripted failure).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD="$HERE/build-ec2-ami.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

# Fake aws. State in $STATE: calls (one line per call), polls (describe
# count), final (the state the build instance ends in; default stopped).
cat >"$TMP/bin/aws" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >>"$STATE/calls"
case "$*" in
*"ssm get-parameter"*) echo ami-0base ;;
*"ec2 run-instances"*)
	for a in "$@"; do case "$a" in fileb://*) gunzip -c "${a#fileb://}" > "$STATE/user-data" && cp "${a#fileb://}" "$STATE/user-data.gz" ;; esac; done
	echo i-0build
	;;
*"ec2 describe-instances"*)
	n=$(($(cat "$STATE/polls" 2>/dev/null || echo 0) + 1))
	echo "$n" >"$STATE/polls"
	if [ "$n" -lt 2 ]; then echo running; else cat "$STATE/final" 2>/dev/null || echo stopped; fi
	;;
*"ec2 wait instance-exists"*) ;;
*"ec2 get-console-output"*) cat "$STATE/console" 2>/dev/null || echo "FIRERUNNER-BUILD 2026-01-01T00:00:00Z finished" ;;
*"ec2 create-image"*) echo ami-0new ;;
*"ec2 describe-images"*) cat "$STATE/image-state" 2>/dev/null || echo available ;;
*"ssm put-parameter"*) ;;
*"ec2 terminate-instances"*) ;;
*) echo "unexpected aws call: $*" >&2; exit 1 ;;
esac
FAKE
# Fake curl: the GitHub API. A runner release's notes carry a SHA-256 per
# asset (aaa… for x64, bbb… for arm64), except v0.0.1's; a runner-images tag
# resolves to 000…7.
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
for last; do :; done
echo "$last" >>"$STATE/curl"
case "$last" in
*/repos/actions/runner/releases/latest) v=9.9.9 ;;
*/repos/actions/runner/releases/tags/v*) v="${last##*/v}" ;;
*/repos/actions/runner-images/commits/*) printf '%040d' 7; exit 0 ;;
*) exit 22 ;;
esac
a="$(printf '%064d' 0 | tr 0 a)" b="$(printf '%064d' 0 | tr 0 b)"
[ "$v" != 0.0.1 ] || a="" b=""
printf '{\n  "tag_name": "v%s",\n  "body": "- <!-- BEGIN SHA linux-x64 -->%s<!-- END SHA linux-x64 -->\\r\\n- <!-- BEGIN SHA linux-arm64 -->%s<!-- END SHA linux-arm64 -->"\n}\n' "$v" "$a" "$b"
FAKE
A64="$(printf '%064d' 0 | tr 0 a)"
B64="$(printf '%064d' 0 | tr 0 b)"
cat >"$TMP/bin/sleep" <<'FAKE'
#!/bin/sh
FAKE
chmod +x "$TMP/bin/"*

fails=0
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then echo "ok   $name"; else echo "FAIL $name: $detail"; fails=$((fails + 1)); fi
}
called() { grep -q -- "$1" "$STATE/calls"; }
not_called() { ! grep -q -- "$1" "$STATE/calls" 2>/dev/null; }

not_grep() { ! grep -q "$@"; }
fresh() {
	STATE="$TMP/state-$1"
	mkdir -p "$STATE"
	export STATE
}
build() {
	PATH="$TMP/bin:$PATH" "$BUILD" --subnet subnet-x --security-group sg-x --ssm-param /firerunner/ami "$@" \
		>"$STATE/out" 2>"$STATE/err" && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
}

# 1. Happy path: builds, publishes, terminates the build instance, prints JSON.
fresh ok
build --tag team=ci
expect "exit 0" "$(cat "$STATE/err")" [ "$(cat "$STATE/rc")" = 0 ]
expect "JSON result" "$(cat "$STATE/out")" \
	[ "$(cat "$STATE/out")" = '{"ami":"ami-0new","ssm_param":"/firerunner/ami","runner_version":"9.9.9","runner_sha256":"'"$A64"'","arch":"x64"}' ]
expect "publishes to the parameter" "$(cat "$STATE/calls")" called "ssm put-parameter --name /firerunner/ami --type String --value ami-0new --overwrite"
expect "terminates the build instance" "" called "ec2 terminate-instances --instance-ids i-0build"
expect "tags everything" "" called "Key=team,Value=ci"
expect "IMDSv2 on the build instance" "" called "HttpTokens=required"
expect "user-data is valid bash" "" bash -n "$STATE/user-data"
expect "user-data pins the runner" "" grep -q '^RUNNER_VERSION=9.9.9$' "$STATE/user-data"
expect "user-data pins the runner's checksum, from the release notes" "" grep -q "^RUNNER_SHA256=$A64\$" "$STATE/user-data"
expect "the runner's version and checksum are logged" "" grep -q "actions/runner 9.9.9 linux-x64 sha256 $A64" "$STATE/err"
# shellcheck disable=SC2016 # a literal $RUNNER_SHA256 in the user-data
expect "user-data checks the runner it downloads" "" \
	bash -c "grep -q '^fetch_checked \"https://github.com/actions/runner/releases/download/' '$STATE/user-data' && grep -q '^	runner.tgz \"\$RUNNER_SHA256\" ||' '$STATE/user-data'"
expect "user-data carries the checks" "" grep -q '^fetch_checked() {$' "$STATE/user-data"
# The build instance cannot run here (images/test-ec2-vm.sh boots one): these
# check that a failed check stops it.
# shellcheck disable=SC2016 # literal $RUNNER_SHA256 and $src in the user-data
expect "a runner that fails its check stops the build" "" grep -qx '	runner.tgz "$RUNNER_SHA256" || {' "$STATE/user-data"
# shellcheck disable=SC2016 # as above
expect "a runner-images fetch that fails stops the build" "" grep -qx '	fetch_runner_images "$src" || return 1' "$STATE/user-data"
expect "user-data pins the default runner-images tag's commit" "" grep -q '^RI_COMMIT=add4380b0d6685806e9192c33c37359ee0aa4023$' "$STATE/user-data"
expect "the default tag is not resolved" "" bash -c "! grep -q runner-images/commits '$STATE/curl'"
expect "user-data installs the boot unit" "" grep -q 'FIRERUNNER_JIT_URL' "$STATE/user-data"
expect "user-data drops cloud-init's netplan" "" grep -q 'rm -f /etc/netplan/50-cloud-init.yaml' "$STATE/user-data"
expect "user-data resets the hostname" "" grep -q 'truncate -s 0 /etc/hostname' "$STATE/user-data"
# The image must keep no copy of the build's user-data or log of its run: a
# --firerunner-url can be presigned.
expect "user-data cleans cloud-init's copies and logs" "" grep -qx 'cloud-init clean --logs --seed' "$STATE/user-data"
expect "user-data removes the instance's user-data" "" grep -q '^rm -rf /var/lib/cloud/instance /var/lib/cloud/instances ' "$STATE/user-data"
expect "user-data cleans up before it reports finished" "" \
	bash -c "[ \"\$(grep -n -e '^cloud-init clean' -e '^mark finished\$' '$STATE/user-data' | cut -d: -f2- | tr '\n' '|')\" = 'cloud-init clean --logs --seed|mark finished|' ]"
expect "user-data fits EC2's limit, compressed" "" [ "$(wc -c <"$STATE/user-data.gz")" -le 16384 ]
expect "user-data installs the cache proxy, inactive" "" \
	bash -c "grep -q 'install_cache_proxy\$' '$STATE/user-data' && grep -q 'systemctl disable nginx.service' '$STATE/user-data' && grep -q '^upstream cache_backend\|server __UPSTREAM__;' '$STATE/user-data'"
# shellcheck disable=SC2016 # a literal $node in the user-data
expect "user-data wraps every bundled node" "" grep -q 'install -m 0755 /usr/local/libexec/nodewrap "\$node"' "$STATE/user-data"

# 2. The build instance dies instead of stopping: no image, no publish, and
# the instance is still terminated.
fresh died
echo terminated >"$STATE/final"
build
expect "fails" "" [ "$(cat "$STATE/rc")" != 0 ]
expect "no image" "" not_called "ec2 create-image"
expect "no publish" "" not_called "ssm put-parameter"
expect "still terminates" "" called "ec2 terminate-instances --instance-ids i-0build"

# 2b. Provisioning failed: the instance powered off with a FAILED mark. No
# image is taken of it.
fresh provision-failed
echo "FIRERUNNER-BUILD 2026-01-01T00:00:00Z FAILED rc=100" >"$STATE/console"
build
expect "fails on a FAILED mark" "$(cat "$STATE/err")" grep -q "provisioning failed" "$STATE/err"
expect "no image of a failed build" "" not_called "ec2 create-image"
expect "terminates a failed build" "" called "ec2 terminate-instances --instance-ids i-0build"

# 2c. Stopped without any mark (console lost): not imaged.
fresh no-mark
echo "some boot noise" >"$STATE/console"
build --timeout 2
expect "fails without a finished mark" "$(cat "$STATE/err")" grep -q "without a 'finished' mark" "$STATE/err"
expect "no image without a mark" "" not_called "ec2 create-image"

# 2d. The image fails: nothing is published.
fresh image-failed
echo failed >"$STATE/image-state"
build
expect "fails on a failed image" "$(cat "$STATE/err")" grep -q "ended as failed" "$STATE/err"
expect "no publish of a failed image" "" not_called "ssm put-parameter"

# 3. Timeout: gives up and terminates.
fresh timeout
echo running >"$STATE/final"
build --timeout 0
expect "times out" "$(cat "$STATE/err")" grep -q "did not finish" "$STATE/err"
expect "terminates on timeout" "" called "ec2 terminate-instances --instance-ids i-0build"

# 4. Bad arguments touch no AWS.
for args in "--arch sparc" "--tag novalue" "--tag team=a,b" "--tag k={x}" "--volume-gib big" "--bogus"; do
	fresh "args-${args// /_}"
	# shellcheck disable=SC2086 # split the case's words on purpose
	build $args
	expect "rejects $args" "" [ "$(cat "$STATE/rc")" = 2 ]
	expect "no AWS for $args" "" not_called "ec2"
done
fresh missing
PATH="$TMP/bin:$PATH" "$BUILD" --subnet subnet-x >"$STATE/out" 2>"$STATE/err" && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "requires --security-group" "" grep -q -- "--security-group is required" "$STATE/err"

# 5. --render needs no AWS and no flags beyond the version.
fresh render
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --arch arm64 >"$STATE/out"
expect "render prints the user-data" "" grep -q '^RUNNER_ARCH=arm64$' "$STATE/out"
expect "render touches no AWS" "" not_called "ec2"
expect "render ships the SYN rule" "" grep -q 'masquerade fully-random' "$STATE/out"
expect "the SYN rule leaves the local cache proxy alone" "" grep -q 'daddr != {.*127.0.0.0/8' "$STATE/out"
expect "render ships the git proxy's unit" "" grep -q '^ExecStopPost=+/usr/local/libexec/firerunner-git-config off$' "$STATE/out"
expect "render ships the git config helper" "" grep -q 'git config --system --remove-section' "$STATE/out"
expect "render installs the binary with its checks" "" grep -q 'sha256sum -c -' "$STATE/out"
expect "the binary is optional, so a build survives without it" "" grep -q '^install_firerunner || mark' "$STATE/out"
expect "render enables its units" "" grep -q '^systemctl enable firerunner-syn-reroll.service firerunner-syn-reroll-stop.timer$' "$STATE/out"
expect "render drops the boot script's comments" "" not_grep 'Powering off ends the instance' "$STATE/out"
expect "render keeps the boot script's commands" "" grep -q 'POWEROFF_FAST:-systemctl poweroff --force' "$STATE/out"

# A binary from a URL must come with its hash.
fresh fr-url
rc=0
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --firerunner-url https://x/y >"$STATE/out" 2>&1 || rc=$?
expect "firerunner url without a hash: refused" "rc=$rc" [ "$rc" != 0 ]
rc=0
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --firerunner-version latest >"$STATE/out" 2>&1 || rc=$?
expect "bad firerunner version: refused" "rc=$rc" [ "$rc" != 0 ]
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --firerunner-version v0.1.1 >"$STATE/out"
expect "firerunner version: reaches the build" "" grep -q '^FR_VERSION=v0.1.1$' "$STATE/out"

# 5b. What the build instance checks its downloads against.
fresh checks
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --arch arm64 >"$STATE/out" 2>"$STATE/err"
expect "arm64 takes the arm64 checksum" "" grep -q "^RUNNER_SHA256=$B64\$" "$STATE/out"
expect "minimal fetches no runner-images" "" grep -q "^RI_COMMIT=''\$" "$STATE/out"
: >"$STATE/curl"
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --runner-sha256 "$B64" >"$STATE/out" 2>"$STATE/err"
expect "a pinned checksum is used" "" grep -q "^RUNNER_SHA256=$B64\$" "$STATE/out"
expect "a pinned checksum reads no release notes" "" bash -c "! grep -q actions/runner/releases '$STATE/curl'"
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --runner-images-ref ubuntu24/other >"$STATE/out" 2>"$STATE/err"
expect "another tag is resolved to its commit" "" grep -q "^RI_COMMIT=$(printf '%040d' 7)\$" "$STATE/out"
expect "and the build says how to pin it" "" grep -q -- "--runner-images-commit $(printf '%040d' 7)" "$STATE/err"
: >"$STATE/curl"
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --runner-images-ref ubuntu24/other --runner-images-commit "$(printf '%040d' 3)" >"$STATE/out" 2>"$STATE/err"
expect "a pinned commit is used" "" grep -q "^RI_COMMIT=$(printf '%040d' 3)\$" "$STATE/out"
expect "a pinned commit resolves no tag" "" bash -c "! grep -q runner-images/commits '$STATE/curl'"
for args in "--runner-sha256 $A64" "--runner-version 1.2.3 --runner-sha256 nothex" \
	"--runner-version 1.2.3 --runner-images-commit main" "--runner-version 0.0.1"; do
	rc=0
	# shellcheck disable=SC2086 # split the case's words on purpose
	PATH="$TMP/bin:$PATH" "$BUILD" --render $args >"$STATE/out" 2>"$STATE/err" || rc=$?
	expect "refuses $args" "rc=$rc $(cat "$STATE/err")" [ "$rc" = 2 ]
	expect "renders nothing for $args" "" [ ! -s "$STATE/out" ]
done
expect "notes without a checksum: says so" "" grep -q "carry no SHA-256 for linux-x64" "$STATE/err"
expect "notes without a checksum: stops there" "" grep -q "cannot check the runner download" "$STATE/err"
rc=0
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-sha256 "$A64" >"$STATE/out" 2>"$STATE/err" || rc=$?
expect "a checksum without a version: says why" "" grep -q -- "--runner-sha256 needs --runner-version" "$STATE/err"

# 6. standard is the default, and full keeps what it skips.
fresh toolset
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 >"$STATE/out"
expect "the default toolset is standard" "" grep -q '^TOOLSET=standard$' "$STATE/out"
expect "the build keeps its scratch out of the snapshot" "" grep -q 'mount -t tmpfs' "$STATE/out"
expect "standard skips Haskell" "" grep -q '^RI_SKIP=.*install-haskell.sh' "$STATE/out"
expect "standard installs PowerShell's modules itself" "" grep -q '^RI_SKIP=.*Install-PowerShellModules.ps1' "$STATE/out"
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --toolset full >"$STATE/out"
expect "full keeps Haskell" "" bash -c "! grep -q '^RI_SKIP=.*install-haskell.sh' '$STATE/out'"
PATH="$TMP/bin:$PATH" "$BUILD" --render --runner-version 1.2.3 --arch arm64 >"$STATE/out"
expect "arm64 defaults to minimal" "" grep -q '^TOOLSET=minimal$' "$STATE/out"
expect "standard on arm64 is refused" "" bash -c "! PATH='$TMP/bin:$PATH' '$BUILD' --render --runner-version 1.2.3 --toolset standard --arch arm64 >/dev/null 2>&1"
fresh toolset-defaults
build
expect "standard gets a build instance that fits" "$(cat "$STATE/calls")" called "--instance-type c7a.4xlarge"
expect "standard gets a disk that fits" "$(cat "$STATE/calls")" called "VolumeSize=40,"
fresh toolset-minimal
build --toolset minimal
expect "minimal stays small" "$(cat "$STATE/calls")" called "--instance-type c7a.large"
fresh toolset-override
build --instance-type m8a.large --volume-gib 25
expect "an explicit type wins" "$(cat "$STATE/calls")" called "--instance-type m8a.large"
expect "an explicit size wins" "$(cat "$STATE/calls")" called "VolumeSize=25,"
expect "a bad toolset is refused" "" bash -c "! PATH='$TMP/bin:$PATH' '$BUILD' --render --runner-version 1.2.3 --toolset huge >/dev/null 2>&1"

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
