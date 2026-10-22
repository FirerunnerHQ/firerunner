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
#               CACHE_UPSTREAM  host:port of a shared cache node; unset runs a
#                            local cache-server with a static identity
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

if [ -z "${CACHE_UPSTREAM:-}" ]; then
  CACHE_UPSTREAM=127.0.0.1:8099
  install -d -m 0750 /run/firerunner/jobs
  cat > /run/firerunner/jobs/127.0.0.1.json <<EOF
{"ip":"127.0.0.1","runner":"poc","generation":"poc-$(date +%s)","repository":"$REPO","event":"push",
 "write_ref":"$REF","read_refs":["$REF"],"run_id":1,"job_id":"poc","expires_at":$(( $(date +%s) + 14400 ))}
EOF
  chmod 0640 /run/firerunner/jobs/127.0.0.1.json

  install -m 0755 "$CACHE_BIN" /usr/local/bin/firerunner
  nohup /usr/local/bin/firerunner cache-server --addr 127.0.0.1:8099 --dir /var/lib/firerunner/cache \
    --jobs-dir /run/firerunner/jobs --artifact-upstream "" > /var/log/firerunner-cache.log 2>&1 &
fi

sed "s|__CACHE_UPSTREAM__|$CACHE_UPSTREAM|" "$POC_DIR/nginx.conf" > /etc/nginx/nginx.conf
nginx -t
systemctl restart nginx

echo "127.0.0.1 $host" >> /etc/hosts

# Node ignores the system store. Wrap the runner's bundled node so every action
# gets the CA, on the host and in job containers alike: externals is mounted
# into containers at /__e, so a path relative to the wrapper holds in both.
cp "$POC_DIR/ca.pem" "$RUNNER_DIR/externals/firerunner-ca.pem"
for bin in "$RUNNER_DIR"/externals/node*/bin/node; do
  [ -e "$bin.real" ] && continue
  mv "$bin" "$bin.real"
  install -m 0755 "$POC_DIR/nodewrap" "$bin"
done

# Job containers resolve through Docker, not /etc/hosts: answer the results
# host with this VM's address from a resolver the daemon hands to containers.
if command -v docker > /dev/null; then
  ip=$(hostname -I | awk '{print $1}')
  command -v dnsmasq > /dev/null || apt-get install -y -q dnsmasq-base
  dnsmasq --listen-address="$ip" --bind-interfaces --no-resolv --no-hosts \
    --server=169.254.169.253 --address="/$host/$ip" --pid-file=/run/firerunner-dnsmasq.pid
  echo "{\"dns\": [\"$ip\"]}" > /etc/docker/daemon.json
  systemctl restart docker
fi

# Bounded: a cache node that is down must not hold the runner back.
for _ in $(seq 25); do curl -sf --max-time 1 "http://$CACHE_UPSTREAM/stats" > /dev/null && break; sleep 0.2; done
curl -sf --resolve "$host:443:127.0.0.1" "https://$host/" -o /dev/null -w 'proxy tls ok: %{http_code}\n' || true
