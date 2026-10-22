# POC: transparent Actions cache interception (no runner patch)

Question: can a self-hosted runner serve `actions/cache` from our own cache
server **without** the cache-redirect golden that renames `ACTIONS_RESULTS_URL`
inside the runner binary, and without touching workflows? This is the approach
Blacksmith describes (an NGINX proxy inside the VM). Answer: **yes**, verified
2026-10-22 on an EC2 runner.

## How it works

1. `results-receiver.actions.githubusercontent.com` resolves to `127.0.0.1`
   through `/etc/hosts`.
2. nginx terminates TLS for that name with a certificate from a POC CA. The CA
   is added to the system store, which the runner (.NET) trusts. Node actions
   get it from `NODE_EXTRA_CA_CERTS`, which is written to the runner's `.env`.
3. nginx routes by path:
   - `/twirp/github.actions.results.api.v1.CacheService/*`, `/upload/*` and
     `/download/*` go to `firerunner cache-server` on `127.0.0.1:8099`.
   - Everything else goes back to GitHub. That covers the runner's step and job
     logs, the live-log WebSocket, `WorkflowStepUpdateService` and
     `ArtifactService`. nginx resolves the real upstream through the VPC
     resolver, so it bypasses the hosts override.
4. cache-server authorizes by source address, as on a falcon. A job record for
   `127.0.0.1` stands in for the identity the daemon derives from `JobStarted`.

Files: `nginx.conf`, `guest-setup.sh`. The EC2 launcher, the TLS material and
the test workflow live outside this repo (kaikhq/manekineko branch
`poc/aws-ephemeral-runner`, `.github/workflows/poc-cache-proxy.yml`).

## Result: run 39730073547

The test used a stock runner (v2.338.0), `actions/cache/save@v4`,
`actions/cache/restore@v4` and `actions/upload-artifact@v4` on c8a.large spot in
us-east-2.

| path | served by | requests | status |
|---|---|---:|---|
| CacheService Create / Finalize / GetDownloadURL | cache-server | 3 | 200 |
| `PUT /upload` (`comp=block` ×3, `comp=blocklist`) | cache-server | 4 | 201 |
| `GET /download` (200 MB) | cache-server | 1 | 200 |
| Receiver step and job logs, WorkflowStepsUpdate | GitHub | 29 | 200 |
| `/_ws/ingest.sock` live logs | GitHub | 1 | 101 |
| ArtifactService Create / Finalize | GitHub | 2 | 200 |

- cache-server stats at the end of the job: `saves 1, hits 1, misses 0,
  bytes 200005289`.
- The payload sha256 matched after the restore, and the step reported
  `cache-hit=true`.
- The artifact reached GitHub (`poc-proxy-proof`, 215 B).
- The job log is complete on GitHub.
- nginx error log: 0 errors.

## Two traps that broke the first attempts

- **`proxy_ssl_verify_depth` defaults to 1.** With it, upstream verification
  fails with `unable to get local issuer certificate`. Every non-cache call then
  returns 502, which silently loses the job's logs on GitHub and fails
  `upload-artifact`. Set it to 3.
- **The live log stream is a WebSocket** (`/_ws/ingest.sock`). The proxy must
  forward `Upgrade` and `Connection`.

## Not covered yet

- **Container jobs (`container:`).** `/etc/hosts` and the CA do not reach the
  job container. It would need a resolver the Docker daemon points containers
  at, plus the CA mounted into the container. `NODE_EXTRA_CA_CERTS` must also be
  valid at the container's path (`/__e`).
- **Identity.** The record here is static. Production derives it per job from
  the scale-set `JobStarted` message, or from a per-job token on a remote cache.
- **Storage.** The cache is on local disk, so it dies with the VM. A shared
  cache needs cache-server on S3 or a remote store. It also needs `/upload` and
  `/download` URLs that stay reachable through the same interception.
- **Trust.** Every job on the VM trusts the POC CA for this one name. The CA key
  must never be on the runner. Scope the leaf certificate to the results host
  only, as here.
