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
#
# and these SSM parameters under $FR_PREFIX/control/:
#   env              FR_* lines (non-secret): FR_URL, FR_APP_CLIENT_ID, ...
#   tiers            the tier catalog (JSON)
#   app-private-key  SecureString: the GitHub App key, or
#   token            SecureString: a PAT (one of the two)
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
	aws autoscaling set-instance-health --instance-id "$instance" --health-status Unhealthy || true
}
trap 'unhealthy $LINENO' ERR

# param NAME: the parameter's value, decrypted; empty when it does not exist.
param() {
	aws ssm get-parameter --name "$FR_PREFIX/control/$1" --with-decryption \
		--query Parameter.Value --output text 2>/dev/null || true
}

install -d -m 0700 "$etc"
umask 077
install -m 0755 "$src/firerunner" "$bin/firerunner"

tiers=$(param tiers)
[ -n "$tiers" ] || { log "missing $FR_PREFIX/control/tiers"; false; }
printf '%s\n' "$tiers" > "$etc/tiers.json"

{
	param env
	echo "FR_TIERS=$etc/tiers.json"
	echo "FR_OWNER=$FR_OWNER"
	echo "FR_AWS_REGION=$AWS_REGION"
	echo "FR_JIT_ADDR=$ip:$FR_JIT_PORT"
	echo "FR_JIT_HOST=$FR_JIT_HOST"
	echo "FR_WORKDIR=/var/lib/firerunner"
} > "$etc/firerunner.env"

key=$(param app-private-key)
token=$(param token)
if [ -n "$key" ]; then
	printf '%s\n' "$key" > "$etc/app-key.pem"
	echo "FR_APP_PRIVATE_KEY=$etc/app-key.pem" >> "$etc/firerunner.env"
elif [ -n "$token" ]; then
	echo "FR_TOKEN=$token" >> "$etc/firerunner.env"
else
	log "missing $FR_PREFIX/control/app-private-key (or token)"
	false
fi

# Refuse to start a config firerunner would reject; the log says why.
# shellcheck source=/dev/null
if ! (set -a && . "$etc/firerunner.env" && "$bin/firerunner" config check); then
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
# Five failed starts in ten minutes: give up, and let the group replace the VM.
StartLimitIntervalSec=10min
StartLimitBurst=5
OnFailure=firerunner-unhealthy.service

[Service]
EnvironmentFile=/etc/firerunner/firerunner.env
ExecStart=/usr/local/bin/firerunner
# Exits are restarts: a deleted scale set or a lost session exits to be
# started fresh.
Restart=always
RestartSec=5
# Jobs run on their own instances and outlive this process; the next control
# VM's reaper ends what they leave. Do not hold a replacement up for them.
TimeoutStopSec=60
StateDirectory=firerunner
StateDirectoryMode=0700
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

cat > "$units/firerunner-unhealthy.service" <<EOF
[Unit]
Description=mark this control VM unhealthy so its Auto Scaling group replaces it

[Service]
Type=oneshot
Environment=AWS_REGION=$AWS_REGION
ExecStart=/usr/bin/aws autoscaling set-instance-health --instance-id $instance --health-status Unhealthy
EOF

"$systemctl" daemon-reload
"$systemctl" enable --now firerunner.service
log "started firerunner $("$bin/firerunner" version) as owner $FR_OWNER"
