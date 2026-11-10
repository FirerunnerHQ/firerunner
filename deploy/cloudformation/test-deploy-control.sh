#!/usr/bin/env bash
# Unit tests for deploy-control.sh. A fake aws plays CloudFormation, SSM and
# Auto Scaling from files in $STATE; a fake curl plays the release server.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/deploy-control.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

# A release asset's content is its name; $STATE/tampered lists the ones
# served with other content. SHA256SUMS is $STATE/sums.
cat > "$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
url="${*: -1}"
echo "$url" >> "$STATE/curl"
[ ! -f "$STATE/release-missing" ] || exit 22
case "$url" in
*/SHA256SUMS) cat "$STATE/sums" ;;
*)
	out="" prev=""
	for a in "$@"; do [ "$prev" = -o ] && out="$a"; prev="$a"; done
	name="${url##*/}"
	if grep -qsx "$name" "$STATE/tampered"; then echo tampered > "$out"; else echo "$name" > "$out"; fi ;;
esac
FAKE
# Fake gh: `gh attestation verify FILE ...` fails for a file named in
# $STATE/unattested, as gh does for a digest with no attestation.
cat > "$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$STATE/gh"
[ "$1 $2" = "attestation verify" ] || { echo "unexpected: gh $*" >&2; exit 9; }
if grep -qsx "${3##*/}" "$STATE/unattested"; then echo "Error: HTTP 404: Not Found" >&2; exit 1; fi
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
	for a in firerunner_linux_amd64 firerunner_linux_arm64 firerunner-control-bootstrap.sh; do
		printf '%s  %s\n' "$(echo "$a" | sha)" "$a"
	done > "$STATE/sums"
	echo "$(echo other | sha)  other.txt" >> "$STATE/sums"
}
sha() { if command -v sha256sum > /dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
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
expect "deploy: pinned the release's checksums" "$(cat "$STATE/ssm/sha256sums")" grep -qx "$(echo firerunner_linux_arm64 | sha)  firerunner_linux_arm64" "$STATE/ssm/sha256sums"
expect "deploy: pinned only the checked files" "$(cat "$STATE/ssm/sha256sums")" [ "$(wc -l < "$STATE/ssm/sha256sums")" -eq 3 ]
expect "deploy: version set" "" grep -qx v2 "$STATE/ssm/version"
for a in firerunner_linux_amd64 firerunner_linux_arm64 firerunner-control-bootstrap.sh; do
	expect "deploy: checked $a's provenance" "$(cat "$STATE/gh")" \
		grep -q "^attestation verify .*/$a --repo acme/fr --signer-workflow acme/fr/.github/workflows/release.yml --source-ref refs/tags/v2 --deny-self-hosted-runners\$" "$STATE/gh"
done

# Provenance: a file without an attestation, or unlike its SHA256SUMS line,
# stops the deploy before it changes anything.
for bad in firerunner-control-bootstrap.sh firerunner_linux_amd64; do
	setup
	echo "$bad" > "$STATE/unattested"
	deploy fr v2
	expect "unattested $bad: exit 1" "$(cat "$STATE/out")" rc 1
	expect "unattested $bad: says so" "$(cat "$STATE/out")" grep -q "$bad: no attestation that acme/fr's release workflow built it for v2" "$STATE/out"
	expect "unattested $bad: shows gh's error" "$(cat "$STATE/out")" grep -q 'HTTP 404' "$STATE/out"
	expect "unattested $bad: nothing pinned" "" bash -c "! grep -q put-parameter '$STATE/aws'"
	expect "unattested $bad: no refresh" "" bash -c "! grep -q start-instance-refresh '$STATE/aws'"
done

setup
echo firerunner_linux_arm64 > "$STATE/tampered"
deploy fr v2
expect "tampered binary: exit 1" "$(cat "$STATE/out")" rc 1
expect "tampered binary: says so" "$(cat "$STATE/out")" grep -q 'firerunner_linux_arm64: SHA-256 is .*, SHA256SUMS says' "$STATE/out"
expect "tampered binary: nothing pinned" "" bash -c "! grep -q put-parameter '$STATE/aws'"

setup
grep -v bootstrap "$STATE/sums" > "$STATE/sums.new" && mv "$STATE/sums.new" "$STATE/sums"
deploy fr v2
expect "no bootstrap in SHA256SUMS: exit 1" "$(cat "$STATE/out")" rc 1
expect "no bootstrap in SHA256SUMS: says so" "$(cat "$STATE/out")" grep -q 'lists no firerunner-control-bootstrap.sh' "$STATE/out"

setup
echo firerunner_linux_amd64 > "$STATE/unattested"
DEPLOY_SKIP_ATTESTATION=1 deploy fr v2
expect "skip attestation: exit 0" "$(cat "$STATE/out")" rc 0
expect "skip attestation: says so" "$(cat "$STATE/out")" grep -q 'DEPLOY_SKIP_ATTESTATION=1: not checking' "$STATE/out"
expect "skip attestation: asks gh nothing" "" bash -c "[ ! -e '$STATE/gh' ]"
expect "skip attestation: still pins the checksums" "$(cat "$STATE/ssm/sha256sums")" [ "$(wc -l < "$STATE/ssm/sha256sums")" -eq 3 ]

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
