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

## Follow-up, verified 2026-10-22

### Container jobs (run 39730793524)

Two things carry the interception into a `container:` job:

- **DNS.** Docker hands job containers its own resolver (`127.0.0.11`), not the
  VM's `/etc/hosts`. A dnsmasq bound to the VM's private address answers the
  results host with that address, and `/etc/docker/daemon.json` points the
  daemon at it.
- **CA.** The runner's bundled node is replaced by `nodewrap`, a static binary
  that sets `NODE_EXTRA_CA_CERTS` to a CA file relative to itself. `externals`
  is mounted into job containers at `/__e`, so the same relative path holds on
  the host and in the container.

The job used `ghcr.io/kaikhq/rails-ci-base:latest` with a redis service. Its
cache requests reached cache-server from the container's address `172.18.0.2`.
Save, restore, the sha256 check and upload-artifact all passed, and nginx logged
0 errors.

⚠️ **The wrapper must not be a shell script.** POSIX sh drops environment
variables whose names are not identifiers. The runner passes action inputs as
`INPUT_<NAME>` with the dashes intact (for example
`INPUT_INCLUDE-HIDDEN-FILES`), so a `#!/bin/sh` wrapper silently loses every
dashed input. `upload-artifact` then fails with `Input does not meet YAML 1.2
"Core Schema"`, and `actions/cache` quietly ignores `fail-on-cache-miss`.

### A shared cache node with per-job identity (runs 39731701940, 39731897383)

- **The node.** One cache-server (`cache-node-setup.sh`) on a private address,
  reachable on 8099 only from the runner security group. Runner VMs set
  `CACHE_UPSTREAM`, and their nginx forwards cache paths to the node.
- **Identity.** The node identifies a guest by its VPC source address, exactly
  as a falcon does. VPC source and destination checks make that address
  unspoofable. The control plane publishes one record per runner VM:
  - pending (a fresh generation) at launch;
  - resolved once the job starts, to `write_ref` = the job's branch and
    `read_refs` = that branch plus the default branch;
  - removed when the instance is gone.

  In this POC the control plane is a script that resolves from the GitHub jobs
  API. Production takes the same fields from the scale-set `JobStarted` message.
- **Across VMs.** `writer` saved 100 MB on 172.31.40.109. `reader` restored it
  on 172.31.38.31 with `cache-hit=true` and a matching sha256.
- **Across refs.** `intruder`, on another branch, restored with
  `restore-keys: poc-shared-` and got `Cache not found`. The node still held an
  entry of the same compression version under that prefix, on the first branch.
  The miss is the ref scope, not a version mismatch.

⚠️ **A host job and a container job do not share entries.**
`rails-ci-base` has no `zstd`, so `actions/cache` falls back to gzip there.
The compression method is part of the cache version, so a host save (zstd) is a
miss for a container restore (gzip). GitHub's own cache behaves the same way.
Add zstd to the image if host and container jobs should share.

## S3 storage and fail-fast, verified 2026-10-22

The cache node runs cache-server from branch `feat/cacheserver-s3`, with
`--s3-bucket` and an instance profile scoped to the prefix.

| scenario | run | result |
|---|---|---|
| writer and reader on different VMs, S3 store | 39776645728 | 300 MB entry in S3 (`blobs/1` plus `index.json`); restored with `cache-hit=true` and a matching sha256 |
| node replaced, then restore only | 39777134486 | a fresh node logged `cache index loaded from store entries=1` and served the same entry, sha256 OK |
| cache node down, save then restore | 39777535606 | save 1 s (`Cache reservation failed: cache unavailable`), restore 1 s (`Cache not found`), job green |

**How fail-fast works.** nginx connects with a 1 s timeout. When the node is
unreachable, a CacheService RPC is answered `200 {"ok":false}` and a blob path
`507`. The toolkit retries a 5xx RPC with backoff, but treats `ok:false` as a
miss or a skipped save and does not retry a 507. cache-server answers 507 the
same way when S3 itself fails.

### Measured: where a cache step's time goes

The breakdown comes from the job logs' millisecond timestamps; containers use
gzip because the image has no zstd.

| | local-disk node, 100 MB | S3 node, 300 MB |
|---|---:|---:|
| restore lookup (`Cache hit for`) | 0.01 s | 0.01 s |
| restore download | 0.23 s (~430 MB/s) | 3.6 s (~83 MB/s) |
| restore extract | 0.28 s | 0.80 s |
| save: tar + gzip | — | 5.6 s |
| save: upload, including the node's put to S3 | — | 1.3 s |

Downloads served from S3 ran at about a fifth of the local-disk rate. The
node now keeps local copies in front of S3 (`--s3-local-cache-size`, from
`feat/cacheserver-s3` 004bdec). Measured on the same 300 MB entry:

| restore | run | source | download | extract |
|---|---|---|---:|---:|
| on the node that saved it | 39781466158 | local copy | 0.39 s (767 MB/s) | 1.02 s |
| first, on a freshly replaced node | 39781977225 | S3, then filled into a local copy in the background (~4 s) | 3.76 s (80 MB/s) | 1.07 s |
| second, on that node | 39782192509 | local copy | 0.53 s (561 MB/s) | 1.15 s |

### Measured: what fail-fast saves (cache node gone, one save plus one restore)

| nginx config | run | save | restore | job |
|---|---|---:|---:|---:|
| fail-fast (`ok:false` / 507, 1 s connect) | 39777535606 | 1 s | 1 s | 22 s |
| 1 s connect, 502 passed through | 39780169408 | 35 s | 36 s | 90 s |
| stock nginx (60 s connect, 502) | 39780112904 | 48 s | 44 s | 112 s |

- **The toolkit's retries.** Without fail-fast, the toolkit makes 5 attempts per
  RPC with backoff. Attempts land about 4, 7, 9 and 13 s apart, so the retries
  alone cost about 35 s per cache step.
- **Why stock nginx was only ~3 s per attempt.** The dead node's address was
  gone from the subnet, so each connect failed on ARP in about 3 s rather than
  on the 60 s connect timeout. **Not measured:** a node that is still up but
  silently drops packets. There each attempt would wait the full connect
  timeout, about 5 × 60 s per step with stock nginx. The 1 s timeout caps this
  case too.

Two traps found on the way:

- **The S3 region.** `aws-sdk-go-v2`'s `LoadDefaultConfig` does not read the
  region from instance metadata unless `config.WithEC2IMDSRegion()` is set.
  Without it, every call failed with `Invalid region`.
- **`s3:ListBucket`.** It is needed even for a single known key: without it, a
  missing `index.json` is a 403, not a 404.

## Still open

- **One live cache-server per prefix.** Ids are server-allocated. Scaling past
  one node needs either shared allocation or sharding by tenant or repository.
- **Trust.** Every job on the VM trusts the POC CA for this one name. The CA key
  must never be on the runner. Scope the leaf certificate to the results host
  only, as here.
