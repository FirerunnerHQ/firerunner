#!/bin/sh
#
# firerunner-cache-setup BUNDLE — point this runner's actions/cache at
# firerunner's cache-server, for a tier that caches. firerunner-ec2-run.sh
# runs it before the runner starts, with the bundle the control VM handed this
# instance once (internal/jitserver CachePath, internal/cachetls Bundle): the
# deployment's CA, this instance's leaf for GitHub's results host, and
# cache-server's host:port.
#
# It makes nginx answer as the results host (firerunner-cache-nginx.conf),
# trusted through the CA by the runner (the system store) and by node actions
# (nodewrap, through externals/firerunner-ca.pem, which job containers see
# too). Job containers resolve through Docker, not /etc/hosts, so a dnsmasq on
# this VM's address answers the results host there.
#
# The /etc/hosts entry comes last: until it is written nothing is diverted,
# so a step that fails leaves the job on GitHub's cache, not on a half-built
# proxy. Exit status is non-zero then, and the caller carries on without it.
set -eu

bundle="${1:?usage: firerunner-cache-setup BUNDLE}"
ROOT="${FIRERUNNER_CACHE_ROOT:-}" # tests work under a scratch root
RUNNER_DIR="${FIRERUNNER_RUNNER_DIR:-/opt/runner}"
DOCKER="${FIRERUNNER_DOCKER:-docker}" # tests name a missing one
HOST=results-receiver.actions.githubusercontent.com
dir="$ROOT/etc/firerunner-cache"

umask 077
install -d -m 0700 "$dir"
jq -er .ca "$bundle" >"$dir/ca.pem"
jq -er .cert "$bundle" >"$dir/server.pem"
jq -er .key "$bundle" >"$dir/server.key"
upstream="$(jq -er .upstream "$bundle")"
# It lands in nginx.conf: a host:port and nothing else.
case "$upstream" in
'' | *[!A-Za-z0-9.:-]* | *:) echo "firerunner-cache-setup: bad upstream '$upstream'" >&2 && exit 1 ;;
*:*) ;;
*) echo "firerunner-cache-setup: upstream '$upstream' has no port" >&2 && exit 1 ;;
esac
chmod 0644 "$dir/ca.pem" "$dir/server.pem"

# Trust: the runner (.NET) reads the system store; node, the file nodewrap
# names. The CA is name-constrained to the results host.
install -d -m 0755 "$ROOT/usr/local/share/ca-certificates"
install -m 0644 "$dir/ca.pem" "$ROOT/usr/local/share/ca-certificates/firerunner-cache.crt"
update-ca-certificates >/dev/null
install -m 0644 "$dir/ca.pem" "$RUNNER_DIR/externals/firerunner-ca.pem"

sed "s|__UPSTREAM__|$upstream|" "$ROOT/etc/firerunner/cache-nginx.conf" >"$ROOT/etc/nginx/nginx.conf"
nginx -t -q
systemctl start nginx

# Job containers: Docker gives them its own resolver, so answer the results
# host from a resolver on this VM's address, and hand that to Docker.
if command -v "$DOCKER" >/dev/null 2>&1; then
	ip="$(hostname -I | awk '{print $1}')"
	[ -n "$ip" ] || { echo "firerunner-cache-setup: no address for job containers" >&2 && exit 1; }
	dnsmasq --listen-address="$ip" --bind-interfaces --no-resolv --no-hosts \
		--server=169.254.169.253 --address="/$HOST/$ip" --pid-file=/run/firerunner-dnsmasq.pid
	daemon="$ROOT/etc/docker/daemon.json"
	[ -s "$daemon" ] || echo '{}' >"$daemon"
	jq --arg ip "$ip" '.dns = [$ip]' "$daemon" >"$daemon.new"
	chmod 0644 "$daemon.new"
	mv "$daemon.new" "$daemon"
	systemctl restart docker
fi

printf '127.0.0.1 %s\n' "$HOST" >>"$ROOT/etc/hosts"
