#!/usr/bin/env bash
# Unit tests for deploy-control.sh. A fake aws plays CloudFormation, SSM and
# Auto Scaling from files in $STATE; a fake curl plays the release server.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/deploy-control.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

cat > "$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
echo "${*: -1}" >> "$STATE/curl"
[ -f "$STATE/release-missing" ] && exit 22
exit 0
FAKE

# Fake aws. $STATE/refresh-statuses: one status per describe, the last repeats.
# $STATE/systemd: what the VM answers (default "active\nfirerunner v2").
cat > "$TMP/bin/aws" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$STATE/aws"
case "$1 $2" in
"cloudformation describe-stacks")
	case "$*" in
	*AutoScalingGroup*) echo fr-control ;;
	*ParameterPrefix*) echo /fr ;;
	*ReleaseRepo*) echo acme/fr ;;
	esac ;;
"ssm get-parameter") echo v1 ;;
"ssm put-parameter") ;;
"autoscaling start-instance-refresh") echo ir-1 ;;
"autoscaling describe-instance-refreshes")
	read -r -a st < "$STATE/refresh-statuses"
	echo "${st[0]}"
	if [ ${#st[@]} -gt 1 ]; then echo "${st[@]:1}" > "$STATE/refresh-statuses"; fi
	;;
"autoscaling describe-auto-scaling-groups") echo i-new ;;
"ssm send-command") echo cmd-1 ;;
"ssm get-command-invocation") printf "$(cat "$STATE/systemd" 2>/dev/null || echo 'active\nfirerunner v2')\n" ;;
*) echo "unexpected: $*" >&2; exit 9 ;;
esac
FAKE
chmod +x "$TMP/bin/"*
printf '#!/bin/sh\n' > "$TMP/bin/sleep"
chmod +x "$TMP/bin/sleep"

fails=0
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then echo "ok   $name"; else echo "FAIL $name: $detail"; fails=$((fails + 1)); fi
}
setup() {
	export STATE="$TMP/state"
	rm -rf "$STATE" && mkdir -p "$STATE"
	echo "Pending InProgress Successful" > "$STATE/refresh-statuses"
}
deploy() {
	local rc=0
	PATH="$TMP/bin:$PATH" DEPLOY_POLL_SECONDS=0 bash "$SCRIPT" "$@" > "$STATE/out" 2>&1 || rc=$?
	echo "$rc" > "$STATE/rc"
}
rc() { [ "$(cat "$STATE/rc")" = "$1" ]; }

setup
deploy fr v2
expect "deploy: exit 0" "$(cat "$STATE/out")" rc 0
expect "deploy: checked the release first" "$(cat "$STATE/curl")" \
	grep -qx https://github.com/acme/fr/releases/download/v2/SHA256SUMS "$STATE/curl"
expect "deploy: set the version" "" grep -q 'ssm put-parameter --name /fr/control/version --type String --value v2 --overwrite' "$STATE/aws"
expect "deploy: terminate-first refresh" "" grep -q '"MinHealthyPercentage":0,"MaxHealthyPercentage":100' "$STATE/aws"
expect "deploy: reports the new VM" "$(cat "$STATE/out")" grep -q 'firerunner v2 active on i-new' "$STATE/out"

setup
touch "$STATE/release-missing"
deploy fr v3
expect "missing release: exit 1" "$(cat "$STATE/out")" rc 1
expect "missing release: version untouched" "" bash -c "! grep -q put-parameter '$STATE/aws'"
expect "missing release: no refresh" "" bash -c "! grep -q start-instance-refresh '$STATE/aws'"

setup
echo "InProgress Failed" > "$STATE/refresh-statuses"
deploy fr v2
expect "refresh failed: exit 1" "$(cat "$STATE/out")" rc 1
expect "refresh failed: says so" "$(cat "$STATE/out")" grep -q 'instance refresh ir-1: Failed' "$STATE/out"

setup
printf 'active\\nfirerunner v1' > "$STATE/systemd"
deploy fr v2
expect "old version running: exit 1" "$(cat "$STATE/out")" rc 1
expect "old version running: says so" "$(cat "$STATE/out")" grep -q 'runs firerunner v1, not v2' "$STATE/out"

setup
printf 'failed\\n' > "$STATE/systemd"
PATH="$TMP/bin:$PATH" DEPLOY_POLL_SECONDS=0 DEPLOY_TIMEOUT_SECONDS=0 bash "$SCRIPT" fr v2 > "$STATE/out" 2>&1 && echo 0 > "$STATE/rc" || echo $? > "$STATE/rc"
expect "never active: times out with exit 1" "$(cat "$STATE/out")" rc 1

setup
deploy fr v2 --no-wait
expect "no-wait: exit 0 once started" "$(cat "$STATE/out")" rc 0
expect "no-wait: did not poll" "" bash -c "! grep -q describe-instance-refreshes '$STATE/aws'"

setup
echo "Pending" > "$STATE/refresh-statuses"
sed -i.bak 's/"autoscaling describe-instance-refreshes")/"autoscaling describe-instance-refreshes") exit 255 ;;\n"x y")/' "$TMP/bin/aws"
deploy fr v2
mv "$TMP/bin/aws.bak" "$TMP/bin/aws"
expect "aws error: exit 1" "$(cat "$STATE/out")" rc 1
expect "aws error: says which step" "$(cat "$STATE/out")" grep -q 'deploy-control: failed: .*aws autoscaling describe-instance-refreshes' "$STATE/out"

setup
deploy fr
expect "usage: exit 2" "$(cat "$STATE/out")" rc 2
deploy fr v2 --bogus
expect "bad flag: exit 2" "$(cat "$STATE/out")" rc 2

if [ "$fails" -gt 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all deploy-control tests passed"
