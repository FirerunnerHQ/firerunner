#!/usr/bin/env bash
# Unit tests for control-bootstrap.sh. Needs bash and coreutils; no network
# and no AWS. Fakes on PATH play IMDS (curl), SSM, Route 53 and Auto Scaling
# (aws), systemd (systemctl) and the firerunner binary; each records what it
# was asked into $STATE.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/control-bootstrap.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

cat > "$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
url="${*: -1}"
case "$url" in
*/latest/api/token) echo tok ;;
*/meta-data/instance-id) echo i-0123 ;;
*/meta-data/local-ipv4) echo 10.1.2.3 ;;
*) exit 22 ;;
esac
FAKE

# Fake aws: SSM parameters are files under $STATE/ssm/<name relative to
# the control path>; other calls append their argv to $STATE/<service>.
cat > "$TMP/bin/aws" <<'FAKE'
#!/usr/bin/env bash
svc="$1"; shift
case "$svc" in
ssm)
	name=""
	while [ $# -gt 0 ]; do [ "$1" = --name ] && name="$2"; shift; done
	f="$STATE/ssm/${name#/fr/control/}"
	[ -f "$f" ] || { echo "ParameterNotFound" >&2; exit 254; }
	cat "$f"
	;;
*) echo "$*" >> "$STATE/$svc" ;;
esac
FAKE

cat > "$TMP/bin/systemctl" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$STATE/systemctl"
FAKE

# The fake binary is what control-bootstrap installs; "config check" records
# the environment it saw and fails when $STATE/check-fails exists.
cat > "$TMP/firerunner" <<'FAKE'
#!/usr/bin/env bash
case "$*" in
version) echo "firerunner v9.9.9" ;;
"config check")
	env | grep -E '^FR_' | sort > "$STATE/check-env"
	[ ! -f "$STATE/check-fails" ] || { echo "config: bad tier" >&2; exit 1; }
	;;
esac
FAKE
chmod +x "$TMP/bin/"* "$TMP/firerunner"

fails=0
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then
		echo "ok   $name"
	else
		echo "FAIL $name: $detail"
		fails=$((fails + 1))
	fi
}

# setup: a fresh root and state with a valid config; callers then change it.
setup() {
	export STATE="$TMP/state" FR_ROOT="$TMP/root"
	rm -rf "$STATE" "$FR_ROOT"
	mkdir -p "$STATE/ssm" "$FR_ROOT/etc/firerunner" "$FR_ROOT/usr/local/bin" "$FR_ROOT/etc/systemd/system"
	cat > "$FR_ROOT/etc/firerunner/bootstrap.env" <<'EOF'
FR_PREFIX=/fr
FR_OWNER=prod-a
FR_ASG=fr-control
FR_ZONE_ID=Z123
FR_JIT_HOST=control.fr.internal
FR_JIT_PORT=8098
AWS_REGION=us-west-2
EOF
	printf 'FR_URL=https://github.com/acme\nFR_APP_CLIENT_ID=Iv1\nFR_APP_INSTALLATION_ID=42\n' > "$STATE/ssm/env"
	echo '[{"name":"cloud","provisioner":"ec2"}]' > "$STATE/ssm/tiers"
	printf -- '-----BEGIN KEY-----\nabc\n-----END KEY-----' > "$STATE/ssm/app-private-key"
}
run() { PATH="$TMP/bin:$PATH" SYSTEMCTL="$TMP/bin/systemctl" bash "$SCRIPT" "$TMP" > "$STATE/out" 2>&1; }
envfile() { cat "$FR_ROOT/etc/firerunner/firerunner.env"; }

# 1. App key: configured, named, started.
setup
expect "app key: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "app key: env carries the JIT address, name, owner and region" "$(envfile)" \
	grep -qx 'FR_JIT_ADDR=10.1.2.3:8098' <(envfile)
for want in FR_JIT_HOST=control.fr.internal FR_OWNER=prod-a FR_AWS_REGION=us-west-2 FR_URL=https://github.com/acme \
	"FR_APP_PRIVATE_KEY=$FR_ROOT/etc/firerunner/app-key.pem"; do
	expect "app key: env has $want" "$(envfile)" grep -qx "$want" <(envfile)
done
expect "app key: no token in the env" "$(envfile)" bash -c "! grep -q FR_TOKEN '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "app key: key file written, owner-only" "$(ls -l "$FR_ROOT/etc/firerunner")" \
	bash -c "[ \"\$(stat -c %a '$FR_ROOT/etc/firerunner/app-key.pem' 2>/dev/null || stat -f %Lp '$FR_ROOT/etc/firerunner/app-key.pem')\" = 600 ]"
expect "app key: tiers written" "" grep -q '"cloud"' "$FR_ROOT/etc/firerunner/tiers.json"
expect "app key: config checked with the final env" "$(cat "$STATE/check-env" 2>/dev/null)" \
	grep -qx "FR_TIERS=$FR_ROOT/etc/firerunner/tiers.json" "$STATE/check-env"
expect "app key: binary installed" "" test -x "$FR_ROOT/usr/local/bin/firerunner"
expect "app key: name points at this instance" "$(cat "$STATE/route53" 2>/dev/null)" \
	grep -q '"Name":"control.fr.internal","Type":"A","TTL":10,"ResourceRecords":\[{"Value":"10.1.2.3"}\]' "$STATE/route53"
expect "app key: unit started" "$(cat "$STATE/systemctl" 2>/dev/null)" grep -qx 'enable --now firerunner.service' "$STATE/systemctl"
expect "app key: unit restarts and marks unhealthy on give-up" "" \
	grep -q 'OnFailure=firerunner-unhealthy.service' "$FR_ROOT/etc/systemd/system/firerunner.service"
expect "app key: unhealthy unit names this instance" "" \
	grep -q 'set-instance-health --instance-id i-0123 --health-status Unhealthy' "$FR_ROOT/etc/systemd/system/firerunner-unhealthy.service"
expect "app key: instance left healthy" "$(cat "$STATE/autoscaling" 2>/dev/null)" test ! -f "$STATE/autoscaling"

# 2. A PAT instead of an app key.
setup
rm "$STATE/ssm/app-private-key"
echo ghp_secret > "$STATE/ssm/token"
expect "token: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "token: env carries it" "$(envfile)" grep -qx 'FR_TOKEN=ghp_secret' <(envfile)
expect "token: no key path" "$(envfile)" bash -c "! grep -q FR_APP_PRIVATE_KEY '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "token: env file owner-only" "" \
	bash -c "[ \"\$(stat -c %a '$FR_ROOT/etc/firerunner/firerunner.env' 2>/dev/null || stat -f %Lp '$FR_ROOT/etc/firerunner/firerunner.env')\" = 600 ]"

# failed <case>: the run failed, marked the instance unhealthy, and neither
# moved the name nor started anything.
failed() {
	local c="$1"
	expect "$c: fails" "it succeeded" bash -c '! run'
	expect "$c: marked unhealthy" "$(cat "$STATE/autoscaling" 2>/dev/null)" \
		grep -q 'set-instance-health --instance-id i-0123 --health-status Unhealthy' "$STATE/autoscaling"
	expect "$c: name untouched" "" test ! -f "$STATE/route53"
	expect "$c: nothing started" "" test ! -f "$STATE/systemctl"
}
export -f run
export TMP SCRIPT

# 3. No catalog.
setup
rm "$STATE/ssm/tiers"
failed "no tiers"
expect "no tiers: says which parameter" "$(cat "$STATE/out")" grep -q 'missing /fr/control/tiers' "$STATE/out"

# 4. No credential.
setup
rm "$STATE/ssm/app-private-key"
failed "no credential"

# 5. firerunner rejects the config.
setup
touch "$STATE/check-fails"
failed "config rejected"
expect "config rejected: firerunner's reason is in the log" "$(cat "$STATE/out")" grep -q 'config: bad tier' "$STATE/out"

# The launch template's user-data: rendered from control.yaml with test
# values, /etc/firerunner moved under the scratch root. A fake release
# server (curl) serves $STATE/release/<file>; the release's bootstrap is a
# stub that records the directory it was handed.
render_userdata() {
	python3 - "$HERE/control.yaml" "$FR_ROOT" > "$TMP/userdata.sh" <<'PY2'
import re, sys
s = open(sys.argv[1]).read()
m = re.search(r"Fn::Base64: !Sub\n            - \|\n(.*?)\n            - ZoneId", s, re.S)
body = "\n".join(l[14:] for l in m.group(1).split("\n"))
vals = {"AWS::Region": "us-west-2", "ParameterPrefix": "/fr", "Owner": "prod-a", "AWS::StackName": "fr",
        "ZoneId": "Z123", "JitName": "control", "ZoneName": "fr.internal", "JitPort": "8098", "ReleaseRepo": "acme/fr"}
body = re.sub(r"\$\{([A-Za-z:]+)\}", lambda m: vals[m.group(1)], body)
print(body.replace("/etc/firerunner", sys.argv[2] + "/etc/firerunner"))
PY2
}
release() {
	mkdir -p "$STATE/release"
	printf '#!/bin/sh\necho elf\n' > "$STATE/release/firerunner_linux_$1"
	# shellcheck disable=SC2016 # expands in the stub
	printf '#!/bin/sh\necho "$1" > "$STATE/bootstrapped"\n' > "$STATE/release/firerunner-control-bootstrap.sh"
	(cd "$STATE/release" && sha256sum firerunner_linux_* firerunner-control-bootstrap.sh > SHA256SUMS)
}
cat > "$TMP/bin/curl-release" <<'FAKE'
#!/usr/bin/env bash
out="" url=""
while [ $# -gt 0 ]; do
	case "$1" in -o) out="$2"; shift ;; -X|-H) shift ;; http*) url="$1" ;; esac
	shift
done
case "$url" in
*/latest/api/token) echo tok; exit ;;
*/meta-data/instance-id) echo i-0123; exit ;;
https://github.com/*/releases/download/*) echo "$url" >> "$STATE/downloads" ;;
*) exit 22 ;;
esac
f="$STATE/release/${url##*/}"
[ -f "$f" ] || exit 22
cp "$f" "$out"
FAKE
chmod +x "$TMP/bin/curl-release"
mkdir -p "$TMP/bin-ud"
for f in aws systemctl; do ln -sf "$TMP/bin/$f" "$TMP/bin-ud/$f"; done
ln -sf "$TMP/bin/curl-release" "$TMP/bin-ud/curl"
# The control VM is Linux on arm64 whatever runs this test.
printf '#!/bin/sh\necho aarch64\n' > "$TMP/bin-ud/uname"
chmod +x "$TMP/bin-ud/uname"
arch=arm64
userdata() { TMPDIR="$TMP" PATH="$TMP/bin-ud:$PATH" bash "$TMP/userdata.sh" > "$STATE/out" 2>&1; }
export -f userdata

{
	# 6. A release that verifies is handed to its bootstrap.
	setup
	echo v1.2.3 > "$STATE/ssm/version"
	release "$arch"
	render_userdata
	expect "user-data: succeeds" "$(cat "$STATE/out" 2>/dev/null)" userdata
	expect "user-data: fetched the version's release" "$(cat "$STATE/downloads" 2>/dev/null)" \
		grep -q "https://github.com/acme/fr/releases/download/v1.2.3/firerunner_linux_$arch" "$STATE/downloads"
	expect "user-data: bootstrap env written" "" grep -qx 'FR_JIT_HOST=control.fr.internal' "$FR_ROOT/etc/firerunner/bootstrap.env"
	expect "user-data: bootstrap ran with the verified binary" "$(cat "$STATE/bootstrapped" 2>/dev/null)" \
		cmp -s "$STATE/release/firerunner_linux_arm64" "$(cat "$STATE/bootstrapped" 2>/dev/null)/firerunner"
	expect "user-data: healthy" "" test ! -f "$STATE/autoscaling"

	# 7. A tampered binary stops the boot and marks the instance unhealthy.
	setup
	echo v1.2.3 > "$STATE/ssm/version"
	release "$arch"
	echo tampered >> "$STATE/release/firerunner_linux_$arch"
	expect "user-data, tampered: fails" "it succeeded" bash -c '! userdata'
	expect "user-data, tampered: bootstrap never ran" "" test ! -f "$STATE/bootstrapped"
	expect "user-data, tampered: marked unhealthy" "" grep -q 'set-instance-health --instance-id i-0123' "$STATE/autoscaling"

	# 8. SHA256SUMS that does not list the binary is no verification.
	setup
	echo v1.2.3 > "$STATE/ssm/version"
	release "$arch"
	grep -v firerunner_linux "$STATE/release/SHA256SUMS" > "$STATE/s" && mv "$STATE/s" "$STATE/release/SHA256SUMS"
	expect "user-data, unlisted: fails" "it succeeded" bash -c '! userdata'
	expect "user-data, unlisted: bootstrap never ran" "" test ! -f "$STATE/bootstrapped"
}

if [ "$fails" -gt 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all control-bootstrap tests passed"
