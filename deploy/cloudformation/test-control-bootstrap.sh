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
	if [ "$1" = get-parameters-by-path ]; then
		echo "$*" >> "$STATE/ssm-paths"
		[ ! -f "$STATE/ssm-path-denied" ] || { echo "An error occurred (AccessDeniedException) when calling the GetParametersByPath operation" >&2; exit 254; }
		[ ! -f "$STATE/ssm-path-throttled" ] || { echo "An error occurred (ThrottlingException) when calling the GetParametersByPath operation: Rate exceeded" >&2; exit 254; }
		echo '{"Parameters": []}'
		exit 0
	fi
	name=""
	while [ $# -gt 0 ]; do [ "$1" = --name ] && name="$2"; shift; done
	f="$STATE/ssm/${name#/fr/control/}"
	[ ! -f "$STATE/ssm-denied" ] || { echo "An error occurred (AccessDeniedException) when calling the GetParameter operation" >&2; exit 254; }
	[ -f "$f" ] || { echo "An error occurred (ParameterNotFound) when calling the GetParameter operation" >&2; exit 254; }
	cat "$f"
	;;
guardduty)
	# One detector, unless $STATE/gd-none; the call is recorded.
	echo "$*" >> "$STATE/guardduty"
	[ ! -f "$STATE/gd-denied" ] || { echo "An error occurred (AccessDeniedException) when calling the ListDetectors operation" >&2; exit 254; }
	[ ! -f "$STATE/gd-throttled" ] || { echo "An error occurred (ThrottlingException) when calling the ListDetectors operation" >&2; exit 254; }
	[ ! -f "$STATE/gd-warn" ] || echo "WARNING: the CLI says something on stderr" >&2
	if [ -f "$STATE/gd-none" ]; then echo 0; else echo 1; fi
	;;
secretsmanager)
	# The secret is $STATE/pool-key; the call is recorded.
	echo "$*" >> "$STATE/secretsmanager"
	[ ! -f "$STATE/sm-denied" ] || { echo "An error occurred (AccessDeniedException) when calling the GetSecretValue operation" >&2; exit 254; }
	cat "$STATE/pool-key"
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

# With the App's key in KMS, the VM is told the key's ARN and fetches no key,
# even one left in SSM.
setup
printf 'FR_APP_KMS_KEY=arn:aws:kms:us-west-2:123456789012:key/0aaa\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
expect "kms key: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "kms key: env names it" "$(envfile)" grep -qx "FR_APP_KMS_KEY=arn:aws:kms:us-west-2:123456789012:key/0aaa" <(envfile)
expect "kms key: no key on disk" "" test ! -e "$FR_ROOT/etc/firerunner/app-key.pem"
expect "kms key: no key in the env" "$(envfile)" bash -c "! grep -q FR_APP_PRIVATE_KEY '$FR_ROOT/etc/firerunner/firerunner.env'"

# A Postgres URL for job events reaches the env file, and only when it is set.
setup
echo "postgres://u:p(w'\$x@db/fr" > "$STATE/ssm/job-events-dsn"
expect "job events: a shell-hostile password still boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "job events: env names the file" "$(envfile)" grep -qx "FR_JOB_EVENTS_DSN_FILE=$FR_ROOT/etc/firerunner/job-events-dsn" <(envfile)
expect "job events: the URL is not in the env file" "$(envfile)" bash -c "! grep -q 'postgres://' '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "job events: the file holds it unchanged" "" grep -qxF "postgres://u:p(w'\$x@db/fr" "$FR_ROOT/etc/firerunner/job-events-dsn"
setup
echo '[{"id":"t1"}]' > "$STATE/ssm/tenants"
expect "tenants: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "tenants: env names the file" "$(envfile)" grep -qx "FR_TENANTS=$FR_ROOT/etc/firerunner/tenants.json" <(envfile)
expect "tenants: the file holds them" "" grep -qxF '[{"id":"t1"}]' "$FR_ROOT/etc/firerunner/tenants.json"
# Tenant suspensions: the daemon reads them from SSM when the role can, and
# a stack whose role cannot leaves them off, with a hint, and still boots.
setup
expect "tenant state: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "tenant state: env names the path" "$(envfile)" grep -qx "FR_TENANT_STATE=ssm:/fr/control/tenant-state" <(envfile)
expect "tenant state: the path probed" "" grep -q -- "--path /fr/control/tenant-state" "$STATE/ssm-paths"
setup
touch "$STATE/ssm-path-denied"
expect "tenant state denied: still boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "tenant state denied: off" "$(envfile)" bash -c "! grep -q FR_TENANT_STATE '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "tenant state denied: says to update the stack" "$(cat "$STATE/out")" grep -q "update the stack to let the VM read it" "$STATE/out"
setup
touch "$STATE/ssm-path-throttled"
expect "tenant state throttled: the boot fails" "$(cat "$STATE/out" 2>/dev/null)" bash -c "! PATH='$TMP/bin:$PATH' SYSTEMCTL='$TMP/bin/systemctl' bash '$SCRIPT' '$TMP' > '$STATE/out' 2>&1"
expect "tenant state throttled: says why" "$(cat "$STATE/out")" grep -q "ThrottlingException" "$STATE/out"
expect "tenant state throttled: marked unhealthy" "$(cat "$STATE/autoscaling" 2>/dev/null)" grep -q "set-instance-health" "$STATE/autoscaling"
# GuardDuty findings flag tenants beside the states: on while the region has a
# detector and the role may read it; off, with a hint, when it may not, or
# when there is no detector; on after any other error, which the daemon
# reports. Never without the states, where the flags are kept.
setup
expect "guardduty: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "guardduty: on" "$(envfile)" grep -qx "FR_GUARDDUTY=true" <(envfile)
setup
touch "$STATE/gd-none"
expect "guardduty no detector: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "guardduty no detector: off" "$(envfile)" bash -c "! grep -q FR_GUARDDUTY '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "guardduty no detector: says so" "$(cat "$STATE/out")" grep -q "GuardDuty has no detector" "$STATE/out"
setup
touch "$STATE/gd-none" "$STATE/gd-warn"
expect "guardduty no detector, a CLI warning: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "guardduty no detector, a CLI warning: off" "$(envfile)" bash -c "! grep -q FR_GUARDDUTY '$FR_ROOT/etc/firerunner/firerunner.env'"
setup
touch "$STATE/gd-denied"
expect "guardduty denied: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "guardduty denied: off" "$(envfile)" bash -c "! grep -q FR_GUARDDUTY '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "guardduty denied: says to update the stack" "$(cat "$STATE/out")" grep -q "update the stack to let the VM read them" "$STATE/out"
setup
touch "$STATE/gd-throttled"
expect "guardduty throttled: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "guardduty throttled: on" "$(envfile)" grep -qx "FR_GUARDDUTY=true" <(envfile)
setup
touch "$STATE/ssm-path-denied"
expect "guardduty without states: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "guardduty without states: off" "$(envfile)" bash -c "! grep -q FR_GUARDDUTY '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "guardduty without states: not asked" "" test ! -f "$STATE/guardduty"
setup
expect "no job events: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "no job events: nothing in the env" "$(envfile)" bash -c "! grep -q FR_JOB_EVENTS '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "no tenants: nothing in the env" "$(envfile)" bash -c "! grep -q FR_TENANTS '$FR_ROOT/etc/firerunner/firerunner.env'"

# The pool key: fetched from its secret into a root-only file the env names,
# never into the env itself, nor the log.
poolsecret=arn:aws:secretsmanager:us-west-2:123456789012:secret:fr-PoolKey-0aaa
poolkey=Kq7Zx2Lm9Pw4Rt6Yv8Bn3Cs5Df1Gh0Jk2Lq4Mw6Ne8Pr0St2Uv4Wx6Yz8Ab0Cd2E
setup
echo "FR_POOL_KEY_SECRET=$poolsecret" >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$poolkey" > "$STATE/pool-key"
expect "pool key: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "pool key: read from its secret" "$(cat "$STATE/secretsmanager" 2>/dev/null)" \
	grep -q -- "get-secret-value --secret-id $poolsecret --query SecretString --output text" "$STATE/secretsmanager"
expect "pool key: env names the file" "$(envfile)" grep -qx "FR_EC2_POOL_KEY_FILE=$FR_ROOT/etc/firerunner/pool-key" <(envfile)
expect "pool key: config checked with it" "$(cat "$STATE/check-env" 2>/dev/null)" \
	grep -qx "FR_EC2_POOL_KEY_FILE=$FR_ROOT/etc/firerunner/pool-key" "$STATE/check-env"
expect "pool key: the file holds it" "" grep -qxF "$poolkey" "$FR_ROOT/etc/firerunner/pool-key"
expect "pool key: file owner-only" "$(ls -l "$FR_ROOT/etc/firerunner")" \
	bash -c "[ \"\$(stat -c %a '$FR_ROOT/etc/firerunner/pool-key' 2>/dev/null || stat -f %Lp '$FR_ROOT/etc/firerunner/pool-key')\" = 600 ]"
expect "pool key: not in the env file, the check's env or the log" "" \
	bash -c "! grep -qF '$poolkey' '$FR_ROOT/etc/firerunner/firerunner.env' '$STATE/check-env' '$STATE/out'"
setup
expect "no pool key: boots" "$(cat "$STATE/out" 2>/dev/null)" run
expect "no pool key: nothing in the env" "$(envfile)" bash -c "! grep -q FR_EC2_POOL_KEY_FILE '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "no pool key: says to update the stack" "$(cat "$STATE/out")" grep -q 'no FR_POOL_KEY_SECRET' "$STATE/out"
expect "no pool key: no secret read" "" test ! -f "$STATE/secretsmanager"

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
cacherole=arn:aws:iam::123456789012:role/fr-CacheRole-0aaa
printf 'FR_CACHE=s3\nFR_CACHE_BUCKET=fr-cache-bucket\nFR_CACHE_ROLE_ARN=%s\nFR_ROOT_GIB=100\n' "$cacherole" >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$cachetiers" > "$STATE/ssm/tiers"
expect "cache s3: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache s3: the daemon assumes the cache role" "$(envfile)" grep -qx "FR_CACHE_ROLE_ARN=$cacherole" <(envfile)
expect "cache s3: and writes its session where the cache user reads it" "$(envfile)" \
	grep -qx 'FR_CACHE_CREDENTIALS_FILE=/run/firerunner/cache-s3-credentials.json' <(envfile)
expect "cache s3: config checked with the cache role" "$(cat "$STATE/check-env" 2>/dev/null)" \
	grep -qx "FR_CACHE_ROLE_ARN=$cacherole" "$STATE/check-env"
expect "cache s3: instances told the cache address" "$(envfile)" grep -qx 'FR_EC2_CACHE_ADDR=control.fr.internal:8099' <(envfile)
expect "cache s3: identities where cache-server reads them" "$(envfile)" grep -qx 'FR_CACHE_JOBS_DIR=/run/firerunner/jobs' <(envfile)
expect "cache s3: config checked with the cache address" "$(cat "$STATE/check-env" 2>/dev/null)" \
	grep -qx 'FR_EC2_CACHE_ADDR=control.fr.internal:8099' "$STATE/check-env"
expect "cache s3: cache user made" "$(cat "$STATE/useradd" 2>/dev/null)" grep -q 'firerunner-cache' "$STATE/useradd"
for want in RuntimeDirectory=firerunner RuntimeDirectoryMode=0750 RuntimeDirectoryPreserve=restart Group=firerunner-cache UMask=0027; do
	expect "cache s3: daemon unit has $want" "" grep -qx "$want" "$units/firerunner.service"
done
expect "cache s3: cache-server on the VPC address, reading the daemon's identities, in S3" "$(cat "$units/firerunner-cache.service" 2>/dev/null)" \
	grep -q 'cache-server --addr 10.1.2.3:8099 --dir /var/lib/firerunner-cache --jobs-dir /run/firerunner/jobs --jobs-owner-uid 0 --max-entry-size 15GB --s3-bucket fr-cache-bucket --s3-prefix firerunner-cache/ --s3-region us-west-2 --s3-local-cache-size 30GB --s3-credentials-file /run/firerunner/cache-s3-credentials.json$' "$units/firerunner-cache.service"
expect "cache s3: no IMDS, so never the control role's credentials" "" grep -qx 'IPAddressDeny=169.254.169.254/32 fd00:ec2::254/128' "$units/firerunner-cache.service"
expect "cache s3: cache-server starts after the daemon that writes its credentials" "" \
	bash -c "grep -qx 'After=firerunner.service' '$units/firerunner-cache.service' && grep -qx 'Wants=firerunner.service' '$units/firerunner-cache.service'"
expect "cache s3: cache-server as its own user, away from the secrets" "" \
	bash -c "grep -qx 'User=firerunner-cache' '$units/firerunner-cache.service' && grep -qx 'InaccessiblePaths=/etc/firerunner' '$units/firerunner-cache.service'"
expect "cache s3: cache-server started" "$(cat "$STATE/systemctl" 2>/dev/null)" grep -qx 'enable --now firerunner-cache.service' "$STATE/systemctl"

# The user is made once, the next VM's bootstrap on a reused image finds it.
rm -f "$STATE/useradd"
expect "cache s3: rerun succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache s3: an existing cache user is kept" "$(cat "$STATE/useradd" 2>/dev/null)" test ! -f "$STATE/useradd"

# A stack made before the cache role: cache-server keeps the instance role.
setup
printf 'FR_CACHE=s3\nFR_CACHE_BUCKET=fr-cache-bucket\nFR_ROOT_GIB=100\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$cachetiers" > "$STATE/ssm/tiers"
expect "cache s3, no role: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache s3, no role: says to update the stack" "$(cat "$STATE/out")" grep -q 'no FR_CACHE_ROLE_ARN' "$STATE/out"
expect "cache s3, no role: no credentials file" "$(cat "$units/firerunner-cache.service" 2>/dev/null)" \
	bash -c "! grep -q -e --s3-credentials-file '$units/firerunner-cache.service' && ! grep -q FR_CACHE_ROLE_ARN '$FR_ROOT/etc/firerunner/firerunner.env'"
expect "cache s3, no role: IMDS reachable, for the instance role" "" bash -c "! grep -q '^IPAddressDeny' '$units/firerunner-cache.service'"

setup
printf 'FR_CACHE=disk\nFR_ROOT_GIB=50\n' >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$cachetiers" > "$STATE/ssm/tiers"
expect "cache disk: succeeds" "$(cat "$STATE/out" 2>/dev/null)" run
expect "cache disk: blobs on disk, capped to a share of the root volume" "$(cat "$units/firerunner-cache.service" 2>/dev/null)" \
	bash -c "grep -q -- '--jobs-owner-uid 0 --max-size 20GB --max-entry-size 7GB$' '$units/firerunner-cache.service' && ! grep -q -- --s3-bucket '$units/firerunner-cache.service'"
expect "cache disk: no IMDS, so no role credentials" "" grep -qx 'IPAddressDeny=169.254.169.254/32 fd00:ec2::254/128' "$units/firerunner-cache.service"

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

# 4c. The pool key cannot be read, or is too short to be one.
setup
echo "FR_POOL_KEY_SECRET=$poolsecret" >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "$poolkey" > "$STATE/pool-key"
touch "$STATE/sm-denied"
failed "pool key denied"
expect "pool key denied: says why" "$(cat "$STATE/out")" grep -q 'AccessDeniedException' "$STATE/out"
setup
echo "FR_POOL_KEY_SECRET=$poolsecret" >> "$FR_ROOT/etc/firerunner/bootstrap.env"
echo "short-pwned" > "$STATE/pool-key"
failed "pool key short"
expect "pool key short: says why, not the key" "$(cat "$STATE/out")" \
	bash -c "grep -q 'fewer than 32' '$STATE/out' && ! grep -q pwned '$STATE/out'"

# 5. firerunner rejects the config.
setup
touch "$STATE/check-fails"
failed "config rejected"
expect "config rejected: firerunner's reason is in the log" "$(cat "$STATE/out")" grep -q 'config: bad tier' "$STATE/out"

# 6. The env parameter is data. Comments, blank lines and quoted values are
# read the way a shell would have read them, and reach both config check and
# the unit's EnvironmentFile unchanged. systemd takes a single-quoted value
# literally ($, backslash and " included; systemd 262, checked 2026-11-10).
setup
printf '%s\n' '# the org' '' 'FR_URL=https://github.com/acme' '  # indented comment' \
	'FR_LABELS="linux, big"' "FR_NAME='fr one'" 'FR_JOB_EVENTS_DSN="postgres://u:p@db/fr?ssl=1&x=a;b"' 'FR_MAX_RUNNERS=' > "$STATE/ssm/env"
printf 'FR_APP_CLIENT_ID=Iv1' >> "$STATE/ssm/env" # no final newline
expect "env data: boots" "$(cat "$STATE/out" 2>/dev/null)" run
for want in FR_URL=https://github.com/acme 'FR_LABELS=linux, big' 'FR_NAME=fr one' \
	'FR_JOB_EVENTS_DSN=postgres://u:p@db/fr?ssl=1&x=a;b' FR_MAX_RUNNERS= FR_APP_CLIENT_ID=Iv1; do
	expect "env data: config check sees $want" "$(cat "$STATE/check-env" 2>/dev/null)" grep -qxF "$want" "$STATE/check-env"
done
for want in "FR_LABELS='linux, big'" "FR_NAME='fr one'" "FR_JOB_EVENTS_DSN='postgres://u:p@db/fr?ssl=1&x=a;b'" FR_MAX_RUNNERS= FR_APP_CLIENT_ID=Iv1; do
	expect "env data: env file has $want" "$(envfile)" grep -qxF "$want" <(envfile)
done
expect "env data: no comment in the env file" "$(envfile)" bash -c "! grep -q '#' '$FR_ROOT/etc/firerunner/firerunner.env'"

# refused <case> <line>: the env parameter's last line is refused: the run
# fails like any other, names the line's number but not its content, and
# nothing in it ran.
refused() {
	local c="$1" line="$2"
	setup
	printf '%s\n' "$line" >> "$STATE/ssm/env"
	failed "$c"
	expect "$c: names the line" "$(cat "$STATE/out")" grep -q '/fr/control/env line 4' "$STATE/out"
	expect "$c: not its content" "$(cat "$STATE/out")" bash -c "! grep -q pwned '$STATE/out'"
	expect "$c: nothing ran" "" test ! -e "$TMP/pwned"
	expect "$c: config never checked" "" test ! -f "$STATE/check-env"
	rm -f "$TMP/pwned"
}
# shellcheck disable=SC2016 # the shell text is the data under test
{
	refused 'env $()' 'FR_X=$(touch "$TMP/pwned")'
	refused 'env backticks' 'FR_X=`touch "$TMP/pwned"`'
	refused 'env ;' 'FR_X=1; touch "$TMP/pwned"'
	refused 'env ; no space' 'FR_X=1;touch'
	refused 'env $ in double quotes' 'FR_X="$(touch "$TMP/pwned")"'
	refused 'env backslash in quotes' "FR_X='a\\b'"
	refused 'env unquoted space' 'FR_X=a touch-pwned'
	refused 'env unmatched quote' 'FR_X="pwned'
	refused 'env non-FR_ key' 'PATH=/pwned'
	refused 'env lowercase key' 'fr_x=pwned'
	refused 'env export' 'export FR_X=pwned'
	refused 'env no =' 'touch "$TMP/pwned"'
}
setup
printf 'FR_X=1\r\n' >> "$STATE/ssm/env"
failed "env carriage return"

# A value this script writes is held to the same rule, and never logged.
setup
rm "$STATE/ssm/app-private-key"
# shellcheck disable=SC2016 # a literal $
echo 'ghp_$pwned' > "$STATE/ssm/token"
failed "token with a \$"
expect "token with a \$: names the setting" "$(cat "$STATE/out")" grep -q 'FR_TOKEN: its value holds' "$STATE/out"
expect "token with a \$: not the token" "$(cat "$STATE/out")" bash -c "! grep -q pwned '$STATE/out'"

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
        "Cache": "s3", "CachePort": "8099", "CacheBucketName": "fr-cache-bucket", "RootVolumeGiB": "30",
        "CacheRoleArn": "arn:aws:iam::123456789012:role/fr-CacheRole-0aaa",
        "AppKMSKeyArn": "", "PoolKey": "arn:aws:secretsmanager:us-west-2:123456789012:secret:fr-PoolKey-0aaa"}
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
		bash -c "grep -qx 'FR_CACHE=s3' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_CACHE_PORT=8099' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_CACHE_BUCKET=fr-cache-bucket' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_ROOT_GIB=30' '$FR_ROOT/etc/firerunner/bootstrap.env' && grep -qx 'FR_CACHE_ROLE_ARN=arn:aws:iam::123456789012:role/fr-CacheRole-0aaa' '$FR_ROOT/etc/firerunner/bootstrap.env'"
	expect "user-data: bootstrap env names the pool key's secret" "$(cat "$FR_ROOT/etc/firerunner/bootstrap.env")" \
		grep -qx 'FR_POOL_KEY_SECRET=arn:aws:secretsmanager:us-west-2:123456789012:secret:fr-PoolKey-0aaa' "$FR_ROOT/etc/firerunner/bootstrap.env"
	expect "user-data: the launch template serves IMDS on IPv4 only" "" \
		grep -q 'MetadataOptions: {.*HttpProtocolIpv6: disabled' "$HERE/control.yaml"
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
