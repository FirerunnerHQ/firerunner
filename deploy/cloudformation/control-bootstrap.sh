#!/usr/bin/env bash
# Configure and start firerunner on a control VM launched by control.yaml.
#
# The launch template's user-data downloads this script, the binary and
# SHA256SUMS from a release, verifies them, and runs:
#
#   control-bootstrap.sh <dir holding the verified firerunner binary>
#
# It reads /etc/firerunner/bootstrap.env (written by user-data):
#   FR_PREFIX     SSM path prefix; config lives under $FR_PREFIX/control/
#   FR_OWNER      the install's firerunner:owner tag (stable across VMs)
#   FR_ASG        this instance's Auto Scaling group
#   FR_ZONE_ID    Route 53 private hosted zone for the JIT name
#   FR_JIT_HOST   the JIT name, e.g. control.firerunner.internal
#   FR_JIT_PORT   the JIT port
#   AWS_REGION    the region
#   FR_CACHE      off, disk or s3: whether this VM runs cache-server for EC2
#                 tiers with ec2.cache, and where it keeps blobs (optional;
#                 default off)
#   FR_CACHE_PORT its port (default 8099)
#   FR_CACHE_BUCKET  with s3, the bucket (prefix firerunner-cache/)
#   FR_CACHE_ROLE_ARN  with s3, the role holding only the bucket's policy: the
#                 daemon assumes it and hands cache-server the session in a
#                 file, so cache-server needs no instance metadata (a stack
#                 made before it leaves it empty: cache-server then uses the
#                 instance role, as before)
#   FR_ROOT_GIB   the root volume's size, which caps the cache's local use
#   FR_APP_KMS_KEY  a KMS key that signs for the GitHub App; set, no key is read
#   FR_POOL_KEY_SECRET  the Secrets Manager secret holding the key that signs
#                 EC2 pool instances (a stack made before it leaves it empty:
#                 firerunner then rebuilds its warm pool after every restart)
#
# and these SSM parameters under $FR_PREFIX/control/:
#   env              FR_* lines (non-secret): FR_URL, FR_APP_CLIENT_ID, ...;
#                    read as data, never by a shell (parse_env)
#   tiers            the tier catalog (JSON)
#   app-private-key  SecureString: the GitHub App key, or
#   token            SecureString: a PAT (one of the two)
#   job-events-dsn   SecureString: Postgres URL that keeps job events (optional)
#   tenants          tenants served beside this install, JSON (optional)
#   tenant-state/<id>  a suspended tenant's record, written by
#                    "firerunner tenant suspend" (optional; the daemon reads
#                    the path itself, every 15 s)
#   tenant-state-flags/<id>  a tenant flagged for review by GuardDuty
#                    findings, written by the daemon (--guardduty), cleared by
#                    "firerunner tenant clear-flag"
#
# On any failure it marks the instance unhealthy, so the group replaces it.
set -euo pipefail

src="${1:?usage: control-bootstrap.sh DIR}"
root="${FR_ROOT:-}" # tests install under a scratch root
etc="$root/etc/firerunner"
bin="$root/usr/local/bin"
units="$root/etc/systemd/system"
systemctl="${SYSTEMCTL:-systemctl}"

log() { echo "control-bootstrap: $*" >&2; }

# shellcheck source=/dev/null
. "$etc/bootstrap.env"
: "${FR_PREFIX:?}" "${FR_OWNER:?}" "${FR_ASG:?}" "${FR_ZONE_ID:?}" "${FR_JIT_HOST:?}" "${FR_JIT_PORT:?}" "${AWS_REGION:?}"
export AWS_REGION

imds() {
	local token
	token=$(curl -fsS -m 5 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 300" http://169.254.169.254/latest/api/token)
	curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/meta-data/$1"
}
instance=$(imds instance-id)
ip=$(imds local-ipv4)

unhealthy() {
	log "failed (line $1); marking $instance unhealthy so the group replaces it"
	aws autoscaling set-instance-health --instance-id "$instance" --health-status Unhealthy \
		--no-should-respect-grace-period || true
}
trap 'unhealthy $LINENO' ERR

# param NAME: the parameter's value, decrypted; empty when it does not exist.
# Any other error (access denied, a KMS key the role cannot use) fails.
param() {
	local out
	if out=$(aws ssm get-parameter --name "$FR_PREFIX/control/$1" --with-decryption \
		--query Parameter.Value --output text 2>&1); then
		printf '%s\n' "$out"
	elif [[ "$out" == *ParameterNotFound* ]]; then
		return 0
	else
		log "read $FR_PREFIX/control/$1: $out"
		return 1
	fi
}

# The daemon's environment, as NAME=value words in order (a later one wins,
# as in systemd). It is data: no shell ever reads it. config check gets it
# through env(1), and the unit through an EnvironmentFile written from it.
envs=()

# Values a shell and systemd both take literally. Plain: letters, digits and
# _ . / : @ % + = , - only. Quoted: anything printable but quotes, backslash,
# $ and backtick, which systemd's EnvironmentFile parser (or a shell) acts on.
plain='^[A-Za-z0-9_./:@%+=,-]*$'
literal() { [[ "$1" != *[[:cntrl:]\'\"\\\$\`]* ]]; }

# setenv NAME VALUE: add a setting this script makes. The value is never
# logged: it may be the token.
setenv() {
	literal "$2" || {
		log "$1: its value holds a quote, backslash, \$, backtick or control character"
		return 1
	}
	envs+=("$1=$2")
}

# parse_env TEXT: add the settings in $FR_PREFIX/control/env. Each line is
# blank, a # comment, or FR_NAME=value, the value plain or in one pair of
# single or double quotes. Anything else fails, naming the line's number but
# not its content, which may hold a secret pasted by mistake.
parse_env() {
	local n=0 line key value
	local name='^(FR_[A-Z0-9_]+)=(.*)$' dq='^"(.*)"$' sq="^'(.*)'\$" skip='^[[:blank:]]*(#.*)?$'
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		[[ "$line" =~ $skip ]] && continue
		if [[ ! "$line" =~ $name ]]; then
			log "$FR_PREFIX/control/env line $n: want FR_NAME=value, the name in A-Z, 0-9 and _"
			return 1
		fi
		key=${BASH_REMATCH[1]} value=${BASH_REMATCH[2]}
		if [[ "$value" =~ $dq || "$value" =~ $sq ]] && literal "${BASH_REMATCH[1]}"; then
			value=${BASH_REMATCH[1]}
		elif [[ ! "$value" =~ $plain ]]; then
			log "$FR_PREFIX/control/env line $n ($key): want a value of letters, digits and _./:@%+=,- or, in quotes, no quote, backslash, \$, backtick or control character"
			return 1
		fi
		envs+=("$key=$value")
	done <<< "$1"
}

# write_env FILE: the settings as systemd's EnvironmentFile reads them back
# unchanged: plain values bare, others in single quotes, inside which systemd
# acts on nothing (and no value holds a single quote).
write_env() {
	local kv
	for kv in "${envs[@]}"; do
		if [[ "${kv#*=}" =~ $plain ]]; then
			printf '%s\n' "$kv"
		else
			printf "%s='%s'\n" "${kv%%=*}" "${kv#*=}"
		fi
	done > "$1"
}

install -d -m 0700 "$etc"
umask 077
install -m 0755 "$src/firerunner" "$bin/firerunner"

tiers=$(param tiers)
[ -n "$tiers" ] || { log "missing $FR_PREFIX/control/tiers"; false; }
printf '%s\n' "$tiers" > "$etc/tiers.json"

env_text=$(param env)
parse_env "$env_text"
setenv FR_TIERS "$etc/tiers.json"
setenv FR_OWNER "$FR_OWNER"
setenv FR_AWS_REGION "$AWS_REGION"
setenv FR_JIT_ADDR "$ip:$FR_JIT_PORT"
setenv FR_JIT_HOST "$FR_JIT_HOST"
setenv FR_WORKDIR /var/lib/firerunner

# The cache: cache-server on this VM, as its own user, reading the job
# identities the daemon writes (README: separate service users). Instances
# are told its address only when a tier caches, or config check would refuse
# the address as a likely typo.
cache="${FR_CACHE:-off}"
cache_port="${FR_CACHE_PORT:-8099}"
cache_creds=/run/firerunner/cache-s3-credentials.json
case "$cache" in
off | disk) ;;
s3) [ -n "${FR_CACHE_BUCKET:-}" ] || { log "FR_CACHE=s3 needs FR_CACHE_BUCKET"; false; } ;;
*) log "FR_CACHE=$cache: want off, disk or s3"; false ;;
esac
cache_tier=$(python3 -c 'import json, sys
print(any((t.get("ec2") or {}).get("cache") for t in json.load(open(sys.argv[1]))))' "$etc/tiers.json")
if [ "$cache" != off ]; then
	id -u firerunner-cache > /dev/null 2>&1 ||
		useradd --system --no-create-home --shell /sbin/nologin firerunner-cache
	setenv FR_CACHE_JOBS_DIR /run/firerunner/jobs
	if [ "$cache" = s3 ] && [ -n "${FR_CACHE_ROLE_ARN:-}" ]; then
		# In the daemon's runtime directory: the cache user's group may read it.
		setenv FR_CACHE_ROLE_ARN "$FR_CACHE_ROLE_ARN"
		setenv FR_CACHE_CREDENTIALS_FILE "$cache_creds"
	elif [ "$cache" = s3 ]; then
		log "no FR_CACHE_ROLE_ARN: cache-server uses the instance role; update the stack to give it a role of its own"
	fi
	if [ "$cache_tier" = True ]; then
		setenv FR_EC2_CACHE_ADDR "$FR_JIT_HOST:$cache_port"
	fi
elif [ "$cache_tier" = True ]; then
	log "a tier sets ec2.cache but the stack's Cache parameter is off: set it to disk or s3"
fi
dsn=$(param job-events-dsn)
if [ -n "$dsn" ]; then
	# In a file of its own: a password may hold anything, and the env file
	# takes only values systemd reads literally.
	printf '%s\n' "$dsn" > "$etc/job-events-dsn"
	setenv FR_JOB_EVENTS_DSN_FILE "$etc/job-events-dsn"
fi
tenants=$(param tenants)
if [ -n "$tenants" ]; then
	printf '%s\n' "$tenants" > "$etc/tenants.json"
	setenv FR_TENANTS "$etc/tenants.json"
fi
# Suspensions live under this path; the daemon reads it every 15 s. A stack
# made before the VM's role could read it leaves suspension off, rather than
# a daemon that cannot read whether its tenants are suspended. Any other
# error fails the boot: a VM that dropped the suspensions on a throttled call
# would serve a suspended tenant.
state_path="$FR_PREFIX/control/tenant-state"
states=''
if out=$(aws ssm get-parameters-by-path --path "$state_path" --max-items 1 2>&1 > /dev/null); then
	setenv FR_TENANT_STATE "ssm:$state_path"
	states=1
elif [[ "$out" == *AccessDenied* ]]; then
	log "cannot read $state_path: tenant suspension is off; update the stack to let the VM read it"
else
	log "reading $state_path: $out"
	false
fi
# GuardDuty findings on the install's instances flag their tenants for review,
# beside the states, while the account has a detector in this region. A flag
# never suspends. A stack made before the VM's role could read GuardDuty
# leaves it off; any other error turns it on, and the daemon's own reads say
# what fails in firerunner doctor.
if [ -n "$states" ]; then
	if out=$(aws guardduty list-detectors --query 'length(DetectorIds)' --output text 2>&1); then
		if [ "$out" != 0 ]; then
			setenv FR_GUARDDUTY true
		else
			log "GuardDuty has no detector in $AWS_REGION: no finding flags a tenant; enable it to have them flagged"
		fi
	elif [[ "$out" == *AccessDenied* ]]; then
		log "cannot read GuardDuty: tenants are not flagged from its findings; update the stack to let the VM read them"
	else
		log "listing GuardDuty detectors: $out; reading findings all the same"
		setenv FR_GUARDDUTY true
	fi
fi
if [ -n "${FR_POOL_KEY_SECRET:-}" ]; then
	# In a root-only file (umask 077), never the env: firerunner reads it.
	pool_key=$(aws secretsmanager get-secret-value --secret-id "$FR_POOL_KEY_SECRET" \
		--query SecretString --output text)
	if [ "${#pool_key}" -lt 32 ]; then
		log "$FR_POOL_KEY_SECRET: the pool key holds fewer than 32 characters"
		false
	fi
	printf '%s\n' "$pool_key" > "$etc/pool-key"
	pool_key=''
	setenv FR_EC2_POOL_KEY_FILE "$etc/pool-key"
else
	log "no FR_POOL_KEY_SECRET: the warm pool is rebuilt after every restart; update the stack to keep it"
fi
key='' token=''
if [ -n "${FR_APP_KMS_KEY:-}" ]; then
	# The key stays in KMS: nothing to fetch, and none on this disk.
	setenv FR_APP_KMS_KEY "$FR_APP_KMS_KEY"
else
	key=$(param app-private-key)
	token=$(param token)
fi
if [ -n "${FR_APP_KMS_KEY:-}" ]; then
	:
elif [ -n "$key" ]; then
	printf '%s\n' "$key" > "$etc/app-key.pem"
	setenv FR_APP_PRIVATE_KEY "$etc/app-key.pem"
elif [ -n "$token" ]; then
	setenv FR_TOKEN "$token"
else
	log "missing $FR_PREFIX/control/app-private-key (or token)"
	false
fi

write_env "$etc/firerunner.env"

# Refuse to start a config firerunner would reject; the log says why.
if ! env "${envs[@]}" "$bin/firerunner" config check; then
	log "firerunner rejected the config"
	false
fi

# Point the JIT name at this instance before any runner is told it.
aws route53 change-resource-record-sets --hosted-zone-id "$FR_ZONE_ID" --change-batch "$(printf \
	'{"Changes":[{"Action":"UPSERT","ResourceRecordSet":{"Name":"%s","Type":"A","TTL":10,"ResourceRecords":[{"Value":"%s"}]}}]}' \
	"$FR_JIT_HOST" "$ip")" > /dev/null
log "$FR_JIT_HOST -> $ip"

cat > "$units/firerunner.service" <<EOF
[Unit]
Description=firerunner control plane
After=network-online.target
Wants=network-online.target
# Ten failed starts in half an hour: give up, and let the group replace the
# VM. firerunner rides out GitHub and AWS outages without exiting, so only a
# process that cannot run gets here.
StartLimitIntervalSec=30min
StartLimitBurst=10
OnFailure=firerunner-unhealthy.service

[Service]
EnvironmentFile=/etc/firerunner/firerunner.env
ExecStart=/usr/local/bin/firerunner
# Exits are restarts: a deleted scale set or a lost session exits to be
# started fresh.
Restart=always
RestartSec=15
# Jobs run on their own instances and outlive this process; the next control
# VM's reaper ends what they leave. Do not hold a replacement up for them.
TimeoutStopSec=60
StateDirectory=firerunner
StateDirectoryMode=0700
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
$(if [ "$cache" != off ]; then
	# The job identities cache-server reads: the daemon's group is the cache
	# user's, and what it writes is not world-readable. A restart keeps the
	# records of jobs still running.
	printf '%s\n' "RuntimeDirectory=firerunner" "RuntimeDirectoryMode=0750" "RuntimeDirectoryPreserve=restart" "Group=firerunner-cache" "UMask=0027"
fi)

[Install]
WantedBy=multi-user.target
EOF

if [ "$cache" != off ] && [ "$cache_tier" != True ]; then
	log "Cache=$cache but no tier sets ec2.cache: not starting cache-server"
elif [ "$cache" != off ]; then
	# The root volume is the daemon's and the journal's too: the cache gets a
	# share of it. On disk, blobs are capped at 40%; with S3, local copies at
	# 30% (S3 holds the rest). One upload stages at most 15%.
	gib="${FR_ROOT_GIB:-8}"
	caps=" --max-size $((gib * 40 / 100))GB --max-entry-size $((gib * 15 / 100))GB"
	# Both of IMDS's addresses: IPv4, and IPv6 should a launch template enable it.
	imds="IPAddressDeny=169.254.169.254/32 fd00:ec2::254/128"
	if [ "$cache" = s3 ]; then
		caps=" --max-entry-size $((gib * 15 / 100))GB --s3-bucket $FR_CACHE_BUCKET --s3-prefix firerunner-cache/ --s3-region $AWS_REGION --s3-local-cache-size $((gib * 30 / 100))GB"
		if [ -n "${FR_CACHE_ROLE_ARN:-}" ]; then
			# The cache role's session, from the daemon; it waits for the file.
			caps+=" --s3-credentials-file $cache_creds"
		else
			# A stack without the cache role: S3 needs the instance role, from IMDS.
			imds="# IMDS stays reachable: no cache role, S3 needs the instance role (see README)"
		fi
	fi
	cat > "$units/firerunner-cache.service" <<EOF
[Unit]
Description=firerunner cache-server for EC2 tiers
After=firerunner.service
Wants=firerunner.service
[Service]
User=firerunner-cache
Group=firerunner-cache
# Bound to the VPC address; the control security group admits the runners'.
ExecStart=/usr/local/bin/firerunner cache-server --addr $ip:$cache_port --dir /var/lib/firerunner-cache --jobs-dir /run/firerunner/jobs --jobs-owner-uid 0$caps
# No instance metadata, so never the control role's credentials: with S3 it
# signs with the cache role's, which the daemon writes to /run/firerunner.
$imds
Restart=always
RestartSec=5
StateDirectory=firerunner-cache
StateDirectoryMode=0700
# The daemon alone owns /run/firerunner, and its secrets stay out of reach.
ReadOnlyPaths=/run
InaccessiblePaths=/etc/firerunner
NoNewPrivileges=true
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
[Install]
WantedBy=multi-user.target
EOF
fi

cat > "$units/firerunner-unhealthy.service" <<EOF
[Unit]
Description=mark this control VM unhealthy so its Auto Scaling group replaces it

[Service]
Type=oneshot
Environment=AWS_REGION=$AWS_REGION
ExecStart=/usr/bin/aws autoscaling set-instance-health --instance-id $instance --health-status Unhealthy --no-should-respect-grace-period
EOF

"$systemctl" daemon-reload
"$systemctl" enable --now firerunner.service
if [ -f "$units/firerunner-cache.service" ]; then
	"$systemctl" enable --now firerunner-cache.service
	log "started cache-server on $ip:$cache_port ($cache)"
fi
log "started firerunner $("$bin/firerunner" version) as owner $FR_OWNER"
