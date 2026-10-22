#!/bin/bash
# Divert the GitHub Actions cache to a local firerunner cache-server without
# patching the runner: the results host resolves to nginx, which terminates TLS
# with a POC CA, serves CacheService and blob paths locally and passes
# everything else (runner logs, ArtifactService) through to GitHub.
#
# Inputs (env): POC_DIR      dir holding ca.pem, server.pem, server.key, nginx.conf
#               CACHE_BIN    path to a linux firerunner binary
#               REPO, REF    the job's repository and ref (a stand-in for the
#                            identity the control plane derives from JobStarted)
#               RUNNER_DIR   actions runner install dir
set -euo pipefail
: "${POC_DIR:?}" "${CACHE_BIN:?}" "${REPO:?}" "${REF:?}" "${RUNNER_DIR:?}"
host=results-receiver.actions.githubusercontent.com

export DEBIAN_FRONTEND=noninteractive
command -v nginx >/dev/null || { apt-get update -q; apt-get install -y -q nginx; }

install -d -m 0755 /etc/firerunner-poc
install -m 0644 "$POC_DIR/server.pem" /etc/firerunner-poc/server.pem
install -m 0600 "$POC_DIR/server.key" /etc/firerunner-poc/server.key
install -m 0644 "$POC_DIR/ca.pem" /usr/local/share/ca-certificates/firerunner-poc.crt
update-ca-certificates > /dev/null

install -d -m 0750 /run/firerunner/jobs
cat > /run/firerunner/jobs/127.0.0.1.json <<EOF
{"ip":"127.0.0.1","runner":"poc","generation":"poc-$(date +%s)","repository":"$REPO","event":"push",
 "write_ref":"$REF","read_refs":["$REF"],"run_id":1,"job_id":"poc","expires_at":$(( $(date +%s) + 14400 ))}
EOF
chmod 0640 /run/firerunner/jobs/127.0.0.1.json

install -m 0755 "$CACHE_BIN" /usr/local/bin/firerunner
nohup /usr/local/bin/firerunner cache-server --addr 127.0.0.1:8099 --dir /var/lib/firerunner/cache \
  --jobs-dir /run/firerunner/jobs --artifact-upstream "" > /var/log/firerunner-cache.log 2>&1 &

install -m 0644 "$POC_DIR/nginx.conf" /etc/nginx/nginx.conf
nginx -t
systemctl restart nginx

echo "127.0.0.1 $host" >> /etc/hosts
# The runner exports this to every step; Node ignores the system store otherwise.
echo "NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt" >> "$RUNNER_DIR/.env"

until curl -sf http://127.0.0.1:8099/stats > /dev/null; do sleep 0.2; done
curl -sf --resolve "$host:443:127.0.0.1" "https://$host/" -o /dev/null -w 'proxy tls ok: %{http_code}\n' || true
