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
	for a in "$@"; do case "$a" in file://*) cp "${a#file://}" "$STATE/user-data" ;; esac; done
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
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
echo '{"tag_name":"v9.9.9"}'
FAKE
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
	[ "$(cat "$STATE/out")" = '{"ami":"ami-0new","ssm_param":"/firerunner/ami","runner_version":"9.9.9","arch":"x64"}' ]
expect "publishes to the parameter" "$(cat "$STATE/calls")" called "ssm put-parameter --name /firerunner/ami --type String --value ami-0new --overwrite"
expect "terminates the build instance" "" called "ec2 terminate-instances --instance-ids i-0build"
expect "tags everything" "" called "Key=team,Value=ci"
expect "IMDSv2 on the build instance" "" called "HttpTokens=required"
expect "user-data is valid bash" "" bash -n "$STATE/user-data"
expect "user-data pins the runner" "" grep -q '^RUNNER_VERSION=9.9.9$' "$STATE/user-data"
expect "user-data installs the boot unit" "" grep -q 'FIRERUNNER_JIT_URL' "$STATE/user-data"
expect "user-data drops cloud-init's netplan" "" grep -q 'rm -f /etc/netplan/50-cloud-init.yaml' "$STATE/user-data"
expect "user-data resets the hostname" "" grep -q 'truncate -s 0 /etc/hostname' "$STATE/user-data"
expect "user-data fits EC2's limit" "" [ "$(wc -c <"$STATE/user-data")" -le 16384 ]

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

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
