#!/bin/bash
# A shared cache node: one cache-server that every runner VM's proxy forwards
# to. Guests are identified by their VPC source address, as on a falcon, from
# records the control plane publishes: pending at launch, resolved from the
# job's repository and ref once it starts.
#
# Inputs (env): CACHE_BIN       path to a linux firerunner binary
#               IDENTITIES_URL  URL of a JSON array of jobidentity.Identity
#               S3_BUCKET, S3_PREFIX  optional: keep blobs and the index in S3
#                               (instance profile credentials)
set -euo pipefail
: "${CACHE_BIN:?}" "${IDENTITIES_URL:?}"

install -m 0755 "$CACHE_BIN" /usr/local/bin/firerunner
install -d -m 0750 /run/firerunner/jobs

cat > /usr/local/bin/firerunner-identity-sync <<'EOF'
#!/usr/bin/env python3
import json, os, sys, tempfile, time, urllib.request

url, jobs = sys.argv[1], sys.argv[2]
while True:
    try:
        want = {r["ip"]: json.dumps(r).encode() for r in json.load(urllib.request.urlopen(url, timeout=5))}
        for ip, data in want.items():
            path = os.path.join(jobs, ip + ".json")
            try:
                if open(path, "rb").read() == data:
                    continue
            except FileNotFoundError:
                pass
            fd, tmp = tempfile.mkstemp(dir=jobs, prefix=".identity-")
            os.write(fd, data)
            os.fchmod(fd, 0o640)
            os.close(fd)
            os.rename(tmp, path)
        for name in os.listdir(jobs):
            if name.endswith(".json") and name[:-5] not in want:
                os.remove(os.path.join(jobs, name))
    except Exception as e:
        print("identity sync:", e, file=sys.stderr, flush=True)
    time.sleep(1)
EOF
chmod 0755 /usr/local/bin/firerunner-identity-sync
nohup /usr/local/bin/firerunner-identity-sync "$IDENTITIES_URL" /run/firerunner/jobs > /var/log/firerunner-identity-sync.log 2>&1 &

s3=()
[ -n "${S3_BUCKET:-}" ] && s3=(--s3-bucket "$S3_BUCKET" --s3-prefix "${S3_PREFIX:-firerunner-cache/}")
nohup /usr/local/bin/firerunner cache-server --addr 0.0.0.0:8099 --dir /var/lib/firerunner/cache \
  --jobs-dir /run/firerunner/jobs --artifact-upstream "" "${s3[@]}" > /var/log/firerunner-cache.log 2>&1 &
until curl -sf http://127.0.0.1:8099/stats > /dev/null; do sleep 0.2; done
