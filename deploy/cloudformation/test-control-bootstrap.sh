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
	[ ! -f "$STATE/ssm-denied" ] || { echo "An error occurred (AccessDeniedException) when calling the GetParameter operation" >&2; exit 254; }
	[ -f "$f" ] || { echo "An error occurred (ParameterNotFound) when calling the GetParameter operation" >&2; exit 254; }
	cat "$f"
	;;
*) echo "$*" >> "$STATE/$svc" ;;
esac
FAKE

cat > "$TMP/bin/systemctl" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$STATE/systemctl"
FAKE

# Fake user database: firerunner-cache exists once useradd made it.
cat > "$TMP/bin/id" <<'FAKE'
#!/usr/bin/env bash
[ "$*" = "-u firerunner-cache" ] && [ -f "$STATE/cache-user" ] && { echo 990; exit 0; }
exit 1
FAKE
cat > "$TMP/bin/useradd" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$STATE/useradd"
touch "$STATE/cache-user"
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

# A Postgres URL for job events reaches the env file, and only when it is set.
setup
echo 'postgres://u:secret@db/fr' > "$STATE/ssm/job-events-dsn"
expect "job events: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "job events: env carries the URL" "$(envfile)" grep -qx 'FR_JOB_EVENTS_DSN=postgres://u:secret@db/fr' <(envfile)
setup
expect "no job events: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "no job events: nothing in the env" "$(envfile)" bash -c "! grep -q FR_JOB_EVENTS_DSN '$FR_ROOT/etc/firerunner/firerunner.env'"

# 2. A PAT instead of an app key.
setup
rm "$STATE/ssm/app-private-key"
echo ghp_secret > "$STATE/ssm/token"
expect "token: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "token: env carries it" "$(envfile)" grep -qx 'FR_TOKEN=ghp_secret' <(envfile)
expect "token: no key path" "$(envfile)" bash -c "! grep -q FR_APP_PRIVATE_KEY '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "token: env file owner-only" "" \
	bash -c "[ \"\$(stat -c %a '$FR_ROOT/etc/firerunner/firerunner.env' 2>/dev/null || stat -f %Lp '$FR_ROOT/etc/firerunner/firerunner.env')\" = 600 ]"

# 3. The cache: cache-server on this VM, told to instances when a tier caches.
units="$TMP/root/etc/systemd/system"
cachetiers='[{"name":"cloud","provisioner":"ec2","ec2":{"cache":true}}]'
setup
printf 'FR_CACHE=s3\nFR_CACHE_BUCKET=fr-cache-bucket\nFR_ROOT_GIB=100\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$cachetiers" > "$STATE/ssm/tiers"
expect "cache s3: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache s3: instances told the cache address" "$(envfile)" grep -qx 'FR_EC2_CACHE_ADDR=control.fr.internal:8099' <(envfile)
expect "cache s3: identities where cache-server reads them" "$(envfile)" grep -qx 'FR_CACHE_JOBS_DIR=/run/firerunner/jobs' <(envfile)
expect "cache s3: config checked with the cache address" "$(cat "$STATE/check-env" 2>/dev/null)" \
	grep -qx 'FR_EC2_CACHE_ADDR=control.fr.internal:8099' "$STATE/check-env"
expect "cache s3: cache user made" "$(cat "$STATE/useradd" 2>/dev/null)" grep -q 'firerunner-cache' "$STATE/useradd"
for want in RuntimeDirectory=firerunner RuntimeDirectoryMode=0750 RuntimeDirectoryPreserve=restart Group=firerunner-cache UMask=0027; do
	expect "cache s3: daemon unit has $want" "" grep -qx "$want" "$units/firerunner.service"
done
expect "cache s3: cache-server on the VPC address, reading the daemon's identities, in S3" "$(cat "$units/firerunner-cache.service" 2>/dev/null)" \
	grep -q 'cache-server --addr 10.1.2.3:8099 --dir /var/lib/firerunner-cache --jobs-dir /run/firerunner/jobs --jobs-owner-uid 0 --max-entry-size 15GB --s3-bucket fr-cache-bucket --s3-prefix firerunner-cache/ --s3-local-cache-size 30GB$' "$units/firerunner-cache.service"
expect "cache s3: IMDS reachable, for S3's credentials" "" bash -c "! grep -q '^IPAddressDeny' '$units/firerunner-cache.service'"
expect "cache s3: cache-server as its own user, away from the secrets" "" \
	bash -c "grep -qx 'User=firerunner-cache' '$units/firerunner-cache.service' && grep -qx 'InaccessiblePaths=/etc/firerunner' '$units/firerunner-cache.service'"
expect "cache s3: cache-server started" "$(cat "$STATE/systemctl" 2>/dev/null)" grep -qx 'enable --now firerunner-cache.service' "$STATE/systemctl"

# The user is made once, the next VM's bootstrap on a reused image finds it.
rm -f "$STATE/useradd"
expect "cache s3: rerun succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache s3: an existing cache user is kept" "$(cat "$STATE/useradd" 2>/dev/null)" test ! -f "$STATE/useradd"

setup
printf 'FR_CACHE=disk\nFR_ROOT_GIB=50\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$cachetiers" > "$STATE/ssm/tiers"
expect "cache disk: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache disk: blobs on disk, capped to a share of the root volume" "$(cat "$units/firerunner-cache.service" 2>/dev/null)" \
	bash -c "grep -q -- '--jobs-owner-uid 0 --max-size 20GB --max-entry-size 7GB$' '$units/firerunner-cache.service' && ! grep -q -- --s3-bucket '$units/firerunner-cache.service'"
expect "cache disk: no IMDS, so no role credentials" "" grep -qx 'IPAddressDeny=169.254.169.254/32' "$units/firerunner-cache.service"

setup
printf 'FR_CACHE=disk\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
expect "cache disk, no cached tier: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache disk, no cached tier: no cache address (config check would refuse it)" "$(envfile)" \
	bash -c "! grep -q FR_EC2_CACHE_ADDR '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "cache disk, no cached tier: no cache-server (it would restart forever without job records)" "$(cat "$STATE/systemctl" 2>/dev/null)" \
	bash -c "[ ! -f '$units/firerunner-cache.service' ] && ! grep -q firerunner-cache.service '$STATE/systemctl'"

setup
expect "cache off: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache off: no cache unit, user or address" "$(ls "$units")" \
	bash -c "[ ! -f '$units/firerunner-cache.service' ] && [ ! -f '$STATE/useradd' ] && ! grep -q -e FR_EC2_CACHE_ADDR -e FR_CACHE_JOBS_DIR '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "cache off: daemon unit unchanged" "" bash -c "! grep -q Group=firerunner-cache '$units/firerunner.service'"

setup
echo "$cachetiers" > "$STATE/ssm/tiers"
run || true
expect "cache off, cached tier: says to set the Cache parameter" "$(cat "$STATE/out")" grep -q "stack's Cache parameter is off" "$STATE/out"

# failed <case>: the run failed, marked the instance unhealthy, and neither
# moved the name nor started anything.
failed() {
	local c="$1"
	expect "$c: fails" "it succeeded" bash -c '! run'
	expect "$c: marked unhealthy, now" "$(cat "$STATE/autoscaling" 2>/dev/null)" \
		grep -q 'set-instance-health --instance-id i-0123 --health-status Unhealthy --no-should-respect-grace-period' "$STATE/autoscaling"
	expect "$c: name untouched" "" test ! -f "$STATE/route53"
	expect "$c: nothing started" "" test ! -f "$STATE/systemctl"
}
export -f run

export TMP SCRIPT
setup
printf 'FR_CACHE=s3\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
failed "cache s3 without a bucket"

setup
printf 'FR_CACHE=memory\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
failed "cache mode unknown"

# 3. No catalog.
setup
rm "$STATE/ssm/tiers"
failed "no tiers"
expect "no tiers: says which parameter" "$(cat "$STATE/out")" grep -q 'missing /fr/control/tiers' "$STATE/out"

# 4. No credential.
setup
rm "$STATE/ssm/app-private-key"
failed "no credential"

# 4b. SSM refuses: not mistaken for a missing parameter.
setup
touch "$STATE/ssm-denied"
failed "ssm denied"
expect "ssm denied: says why" "$(cat "$STATE/out")" grep -q 'AccessDeniedException' "$STATE/out"
expect "ssm denied: not reported as missing" "$(cat "$STATE/out")" bash -c "! grep -q missing '$STATE/out'"

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
        "ZoneId": "Z123", "JitName": "control", "ZoneName": "fr.internal", "JitPort": "8098", "ReleaseRepo": "acme/fr",
        "Cache": "s3", "CachePort": "8099", "CacheBucketName": "fr-cache-bucket", "RootVolumeGiB": "30"}
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
	cp "$STATE/release/SHA256SUMS" "$STATE/ssm/sha256sums" # pinned at deploy
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
	expect "user-data: bootstrap env names the cache mode and bucket" "$(cat "$FR_ROOT/etc/firerunner/bootstrap.env")" \
		bash -c "grep -qx 'FR_CACHE=s3' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_CACHE_PORT=8099' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_CACHE_BUCKET=fr-cache-bucket' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_ROOT_GIB=30' '$FR_ROOT/etc/firerunner/bootstrap.env'"
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
	expect "user-data, tampered: marked unhealthy, now" "" grep -q 'set-instance-health --instance-id i-0123 --health-status Unhealthy --no-should-respect-grace-period' "$STATE/autoscaling"

	# 8. SHA256SUMS that does not list the binary is no verification.
	setup
	echo v1.2.3 > "$STATE/ssm/version"
	release "$arch"
	grep -v firerunner_linux "$STATE/ssm/sha256sums" > "$STATE/s" && mv "$STATE/s" "$STATE/ssm/sha256sums"
	expect "user-data, unlisted: fails" "it succeeded" bash -c '! userdata'
	expect "user-data, unlisted: bootstrap never ran" "" test ! -f "$STATE/bootstrapped"

	# 9. A release whose assets and SHA256SUMS were both replaced after the
	# deploy: the pinned checksums still refuse it.
	setup
	echo v1.2.3 > "$STATE/ssm/version"
	release "$arch"
	echo swapped >> "$STATE/release/firerunner_linux_$arch"
	(cd "$STATE/release" && sha256sum firerunner_linux_* firerunner-control-bootstrap.sh > SHA256SUMS)
	expect "user-data, replaced release: fails" "it succeeded" bash -c '! userdata'
	expect "user-data, replaced release: bootstrap never ran" "" test ! -f "$STATE/bootstrapped"
	# shellcheck disable=SC2016 # expands in the child
	expect "user-data, replaced release: never fetched the release's SHA256SUMS" "$(cat "$STATE/downloads" 2>/dev/null)" \
		bash -c '! grep -q SHA256SUMS "$STATE/downloads"'
}

if [ "$fails" -gt 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all control-bootstrap tests passed"
