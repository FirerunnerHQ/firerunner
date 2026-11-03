#!/bin/sh
#
# firerunner-cache-setup BUNDLE — point this runner's actions/cache at
# firerunner's cache-server, for a tier that caches. firerunner-ec2-run.sh
# runs it before the runner starts, with the bundle the control VM handed this
# instance once (internal/jitserver CachePath, internal/cachetls Bundle): the
# deployment's CA, this instance's leaf for GitHub's results host, and
# cache-server's host:port.
#
# It makes nginx, on loopback only, answer as the results host
# (firerunner-cache-nginx.conf), trusted through the CA by the runner (the
# system store) and by node actions (nodewrap, through
# externals/firerunner-ca.pem).
#
# Only the host is diverted: containers resolve through Docker and keep
# GitHub's cache. Diverting them all would send every container's calls to the
# results host here, buildkit's (docker/build-push-action, cache-to type=gha)
# among them, whose own CA store refuses this CA: a build that works on
# GitHub's cache would fail. (#140 tracks caching container jobs.)
#
# The /etc/hosts entry comes last: until it is written nothing is diverted,
# so a step that fails leaves the job on GitHub's cache, not on a half-built
# proxy. Exit status is non-zero then, and the caller carries on without it.
set -eu

bundle="${1:?usage: firerunner-cache-setup BUNDLE}"
ROOT="${FIRERUNNER_CACHE_ROOT:-}" # tests work under a scratch root
RUNNER_DIR="${FIRERUNNER_RUNNER_DIR:-/opt/runner}"
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

printf '127.0.0.1 %s\n' "$HOST" >>"$ROOT/etc/hosts"
