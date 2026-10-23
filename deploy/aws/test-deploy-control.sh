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
[ ! -f "$STATE/release-missing" ] || exit 22
printf 'aaa  firerunner_linux_amd64\nbbb  firerunner_linux_arm64\nccc  firerunner-control-bootstrap.sh\n'
FAKE

# Fake aws. $STATE/refresh-statuses: one status per describe, the last repeats.
# $STATE/probes: one answer per line and probe of the VM, the last repeats;
# an answer is state,version,restarts,uptime-seconds. $STATE/refresh-fails: the
# refresh does not start. SSM parameters live in $STATE/ssm/<name>.
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
"ssm get-parameter")
	name=""; for a in "$@"; do [ "${prev:-}" = --name ] && name="$a"; prev="$a"; done
	cat "$STATE/ssm/${name##*/}" 2>/dev/null || exit 254 ;;
"ssm put-parameter")
	name="" value=""; prev=""
	for a in "$@"; do
		case "$prev" in --name) name="$a" ;; --value) value="$a" ;; esac
		prev="$a"
	done
	printf '%s\n' "$value" > "$STATE/ssm/${name##*/}" ;;
"autoscaling start-instance-refresh")
	[ ! -f "$STATE/refresh-fails" ] || { echo "InstanceRefreshInProgress" >&2; exit 254; }
	echo ir-1 ;;
"autoscaling describe-instance-refreshes")
	[ ! -f "$STATE/describe-fails" ] || exit 255
	read -r -a st < "$STATE/refresh-statuses"
	echo "${st[0]}"
	if [ ${#st[@]} -gt 1 ]; then echo "${st[@]:1}" > "$STATE/refresh-statuses"; fi
	;;
"autoscaling describe-auto-scaling-groups") echo i-new ;;
"ssm send-command") echo cmd-1 ;;
"ssm get-command-invocation")
	head -1 "$STATE/probes" | tr , '\n'
	if [ "$(wc -l < "$STATE/probes")" -gt 1 ]; then sed -i.bak 1d "$STATE/probes"; fi ;;
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
	echo "active,firerunner v2,0,61" > "$STATE/probes"
	mkdir -p "$STATE/ssm"
	echo v1 > "$STATE/ssm/version"
	echo "old  firerunner_linux_arm64" > "$STATE/ssm/sha256sums"
}
deploy() {
	local rc=0
	PATH="$TMP/bin:$PATH" DEPLOY_POLL_SECONDS=0 DEPLOY_TIMEOUT_SECONDS=5 bash "$SCRIPT" "$@" > "$STATE/out" 2>&1 || rc=$?
	echo "$rc" > "$STATE/rc"
}
rc() { [ "$(cat "$STATE/rc")" = "$1" ]; }

setup
deploy fr v2
expect "deploy: exit 0" "$(cat "$STATE/out")" rc 0
expect "deploy: checked the release first" "$(cat "$STATE/curl")" \
	grep -qx https://github.com/acme/fr/releases/download/v2/SHA256SUMS "$STATE/curl"
expect "deploy: terminate-first refresh" "" grep -q '"MinHealthyPercentage":0,"MaxHealthyPercentage":100' "$STATE/aws"
expect "deploy: reports the new VM" "$(cat "$STATE/out")" grep -q 'firerunner v2 active on i-new' "$STATE/out"
expect "deploy: pinned the release's checksums" "$(cat "$STATE/ssm/sha256sums")" grep -q 'bbb  firerunner_linux_arm64' "$STATE/ssm/sha256sums"
expect "deploy: version set" "" grep -qx v2 "$STATE/ssm/version"

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
echo "active,firerunner v1,0,500" > "$STATE/probes"
deploy fr v2
expect "old version running: exit 1" "$(cat "$STATE/out")" rc 1
expect "old version running: says so" "$(cat "$STATE/out")" grep -q 'runs firerunner v1, not v2' "$STATE/out"

setup
printf '%s\n' "activating,firerunner v2,0,0" "active,firerunner v2,0,10" "active,firerunner v2,0,70" > "$STATE/probes"
deploy fr v2
expect "settling: waits for a minute of uptime" "$(cat "$STATE/out")" rc 0
expect "settling: reports it" "$(cat "$STATE/out")" grep -q 'active on i-new for 70s' "$STATE/out"

setup
printf '%s\n' "active,firerunner v2,0,3" "active,firerunner v2,3,4" > "$STATE/probes"
deploy fr v2
expect "crash loop: exit 1" "$(cat "$STATE/out")" rc 1
expect "crash loop: says so" "$(cat "$STATE/out")" grep -q 'restarted 3 time(s)' "$STATE/out"

setup
echo "failed,firerunner v2,0,0" > "$STATE/probes"
echo "Successful" > "$STATE/refresh-statuses"
PATH="$TMP/bin:$PATH" DEPLOY_POLL_SECONDS=0 DEPLOY_TIMEOUT_SECONDS=1 bash "$SCRIPT" fr v2 > "$STATE/out" 2>&1 && echo 0 > "$STATE/rc" || echo $? > "$STATE/rc"
expect "never active: exit 1" "$(cat "$STATE/out")" rc 1
expect "never active: times out on the VM, not the refresh" "$(cat "$STATE/out")" grep -q 'timed out: firerunner not settled on i-new (failed)' "$STATE/out"

setup
touch "$STATE/refresh-fails"
deploy fr v2
expect "refresh not started: exit 1" "$(cat "$STATE/out")" rc 1
expect "refresh not started: version restored" "$(cat "$STATE/ssm/version")" grep -qx v1 "$STATE/ssm/version"
expect "refresh not started: checksums restored" "$(cat "$STATE/ssm/sha256sums")" grep -q old "$STATE/ssm/sha256sums"

setup
deploy fr v2 --no-wait
expect "no-wait: exit 0 once started" "$(cat "$STATE/out")" rc 0
expect "no-wait: did not poll" "" bash -c "! grep -q describe-instance-refreshes '$STATE/aws'"

setup
echo "Pending" > "$STATE/refresh-statuses"
touch "$STATE/describe-fails"
deploy fr v2
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
