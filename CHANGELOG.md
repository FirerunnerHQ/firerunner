# Changelog

All notable changes to firerunner are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.22] - 2026-11-13

### Added

- With `FR_JOB_EVENTS_DSN` set, each job a runner runs now gets one usage
  event in the job store, within about 15 seconds of completing.
  - **What it holds:** the job's tenant, repository, workflow, tier, machine,
    and its run time in milliseconds.
  - **Written once:** its ID stays the same however often the job's events
    arrive.
  - **Reading them:** `firerunner usage --after CURSOR` prints the events in
    order, one JSON object a line, for billing or chargeback to read.
    `proprietary/jobstore/usage-event.schema.json` describes an event.

### Changed

- Job events, and the jobs and runners kept from them, now name their
  tenant. Two tenants' runners with the same name no longer share a row.
  Rows written before the upgrade belong to the install.
- The job store's migration is one-way: after it, an older firerunner can
  no longer write to the store. Do not roll back while keeping it.

## [0.1.21] - 2026-11-13

### Changed

- A panic in one installation's start, stop or tiers' listeners no longer
  ends the daemon: that installation stops, drains and starts again by
  itself, as after any other failure, while the others keep serving.
  `firerunner status` counts each installation's restarts, panics and last
  error, and `firerunner doctor` warns while one has panicked
  (`tenant-panics`).

### Fixed

- An installation removed from the tenants list while it was not being
  served (waiting to start again after a failure, or not served as none of
  its tiers is within its limits) now has its scale sets deleted too, by
  name, as one removed while serving does.

## [0.1.20] - 2026-11-12

### Added

- An entitlement record can carry its plan's limits: `max_runners`, at most
  that many runners at once across an installation's tiers (the narrower of
  this and the tenants list's holds), and `tiers`, the only tiers it launches
  on. A change applies within 30 seconds while the installation runs, and
  stops no runner. `firerunner entitlement set` takes `--max-runners` and
  `--tiers`; `firerunner status` shows the plan's limits and the limit in
  force.
- `firerunner status` counts, per tier, the jobs held since the daemon
  started, by what held them: the installation's own runner limit, its
  plan's, a tier its plan leaves out, a hold or a suspension (`held_jobs` in
  `--json`). Each entitlement record shows when the store last saw it
  written, and an installation launching on an expired record shows since
  when and how many times it has begun to (`fail_open_since`, `fail_opens`);
  the daemon logs a warning each time one begins.

## [0.1.19] - 2026-11-12

### Added

- The tenants list (`--tenants`, `FR_TENANTS`) can be an SSM parameter
  (`ssm:/name`), and the daemon reads it again every 30 seconds: installations
  added start, removed ones finish their jobs and have their scale sets
  deleted, and changed ones restart as listed once their runners finish. A
  change to `max_runners` alone applies at once. A list that does not read or
  check changes nothing, and `firerunner doctor` warns (`tenant-catalog`); so
  does one that has disappeared, once a list has been read: removing every
  installation takes `[]`.
- The CloudFormation control VM reads its tenants from
  `<prefix>/control/tenants` itself, so adding an installation needs no new VM.

### Changed

- GuardDuty findings flag an installation listed now or still finishing its
  jobs after removal, and any installation under the owner tag until the
  first list is read.

## [0.1.18] - 2026-11-12

### Added

- `--entitlements` (`FR_ENTITLEMENTS`) reads, every 30 seconds, what billing
  says each tenant may run: its account state (`trial`, `active`, `past_due`,
  `suspended`), its budget state (`ok`, `warning`, `capped`) and until when.
  A tenant with no record, a suspended account or a capped budget is held:
  its jobs wait, recorded as `held` with the reason, and run once a record
  lets them; running jobs go on. An expired record holds a trial or past-due
  tenant until billing renews it, and lets an active one go on launching
  (unless capped). The install is never held. `firerunner status` shows each
  tenant's record and what it allows; `doctor` warns while reads fail, while
  a tenant launches on an expired record, or has none. Off unless set.
- `firerunner entitlement set|clear|list` writes and lists those records, as
  billing would, in a file or SSM.
- CloudFormation: the control VM may read `<prefix>/control/entitlements`;
  set `FR_ENTITLEMENTS` in the `env` parameter to turn them on.

## [0.1.17] - 2026-11-12

### Changed

- With `--ec2-pool-key-file`, the EC2 reaper ends an instance only if a MAC
  under the key proves this install launched it, and cancels a spot request
  only by its own MAC. RunInstances now tags each instance
  `firerunner:launch-mac`, over the launch's ClientToken, so an instance is
  provable from its first moment; its pool or runner MAC, or its spot
  request's, proves it too. The pool signs each persistent spot request
  (`firerunner:request-mac`), including those of instances it takes in
  after the upgrade. What carries the owner tag but no such MAC is left
  alone: `firerunner status` lists it, and `firerunner doctor` warns
  (`ec2-reaper`). With the key, a pool instance whose MACs all fail is no
  longer terminated, so changing the pool key leaves the old pool for you
  to end by hand.
- CloudFormation: `reaper.yaml` takes `PoolKeySecret` (`control.yaml`'s new
  output of that name); given it, the Lambda ends only instances a MAC
  proves the install's, and logs the rest as `unproven`.

## [0.1.16] - 2026-11-11

### Added

- `--guardduty` (`FR_GUARDDUTY`) reads AWS GuardDuty's findings on the
  install's EC2 instances, in its own account, every 2 minutes, and flags for
  review the tenant whose machine a finding names, by its `firerunner:owner`
  tag, from `--guardduty-min-severity` (default 4, Medium) up. A flag never
  suspends. `firerunner status` shows it, `doctor` warns while any tenant is
  flagged, the log warns once per finding and per update of one, and job events
  record each as `flagged`. Flags are kept beside `--tenant-state`, under its
  name with `-flags` added, with how far the daemon has read, so a restart does
  not count a finding again.
- `firerunner tenant clear-flag ID` clears a flag once reviewed; `tenant list`
  shows flags beside suspensions; `tenant findings` lists the findings on an
  owner's instances and the tenant each names, and writes nothing. Where AWS
  refuses a `suspend`, `resume` or `clear-flag`, as on a control VM, the
  command says to run it with operator credentials, and prints it.
- CloudFormation: the control VM reads GuardDuty findings while the region has
  a detector at boot, and writes flags under
  `<prefix>/control/tenant-state-flags`; update the stack to give its role
  those grants.

## [0.1.15] - 2026-11-11

### Added

- cache-server counts the connections its caps refuse, by cap, and the
  uploads and downloads `--body-idle-timeout` cuts, by direction, in `/stats`
  and `/metrics` (`firerunner_cache_conn_refused_total{cap}`,
  `firerunner_cache_idle_cuts_total{direction}`), and logs each kind at most
  once a minute. The EC2 JIT endpoint counts its refusals too.
  `firerunner status` shows both, and `firerunner doctor` warns for an hour
  after the latest refusal.
- Each tenant in the `--tenants` catalog runs under limits: by default at most 2
  runners at once across its tiers, only tiers of at most 2 vCPU, and 30 minutes
  per job. Raise them in its entry's `limits` (`max_runners`, `max_vcpu`,
  `max_job_duration`; 0 lifts one). A job past the runner limit waits, and
  `firerunner status` shows why; a job past its duration has its runner ended;
  a tier above the vCPU limit is not served to that tenant, with a warning.
  `firerunner config check` shows each tenant's limits, and the job events
  record `held` jobs and `stopped` runners with the reason. The install's own
  tiers take limits from `--limit-runners`, `--limit-vcpu` and
  `--limit-job-duration`, and have none by default.
- `firerunner tenant suspend ID --reason TEXT`, `tenant resume ID` and `tenant
  list` keep which tenants are suspended where the daemon's new
  `--tenant-state` (`FR_TENANT_STATE`) names: a JSON file, or `ssm:` and an SSM
  path. The daemon reads it at start and every 15 seconds; a suspended tenant's
  launches are held, and every runner it has is ended at once, busy or not. Its
  reaper ends one a restart left behind. `firerunner status` shows the reason
  and the time, and `doctor` warns while the states cannot be read.
- CloudFormation: the control VM reads suspensions from
  `<prefix>/control/tenant-state`; update the stack to give its role that read.

### Changed

- Upgrading with a `--tenants` catalog: a tenant entry without `limits` now
  runs under the defaults above, so a tenant on tiers of more than 2 vCPU stops
  being served them, and its jobs past 30 minutes end. Add `limits` to each
  entry that needs more before upgrading; `firerunner config check` notes every
  tenant that sets none.

### Fixed

- `firerunner status`, `doctor` and local scrapers reach cache-server's
  `/stats` and `/metrics` even when guests fill its connection caps:
  connections from the host itself are no longer capped.
- `firerunner tenant` reads the daemon's settings file for its state store
  when no `FR_*` variable is set, as `status` and `doctor` do, so on a control
  VM `firerunner tenant list` needs no `--state`; `--env-file` chooses another
  file or none.

### Security

- The EC2 reaper deregisters a runner at GitHub only when the instance's
  runner tag is signed by this install: firerunner now tags each job instance
  `firerunner:runner-mac`, an HMAC under the `--ec2-pool-key-file` key. A
  running instance without a valid one, including one still running from a
  release before this, is left to `--max-vm-lifetime`. Without a key file, a
  restarted firerunner deregisters no runner an earlier process launched;
  `doctor` and the startup log now warn of a missing key on any EC2 install,
  not only one with a pool.
- Before handing a pool instance a job, firerunner checks that its image and
  EBS volumes are still the ones it was launched with (signed in the new
  `firerunner:pool-disk-mac` tag), that its security groups are exactly the
  tier's, its subnet one of the tier's, its type the one it was launched as,
  and that it has no instance profile, one network interface, no public
  address and IMDSv2-only metadata. One that differs is never started, and
  is terminated. **Upgrading discards existing pool instances once:** they
  carry no disk MAC, so they are terminated and the pool refills.
- `network.yaml`'s runner subnets deny outbound SMTP (TCP 25, 465 and 587)
  through a network ACL, so job code on an EC2 runner cannot send mail; a
  security group can only allow. A VPC of your own needs the same.

## [0.1.14] - 2026-11-10

### Changed

- cache-server's `--body-idle-timeout` (default 60s) now also ends a response
  the client stops reading for that long, a download above all (a megabyte
  must be read within each period); a transfer that keeps moving is never
  cut. One address may hold at most 64 connections to the cache server, and
  all of them together 4096.

### Security

- EC2 pool instances are signed: firerunner tags each one it launches
  `firerunner:pool-mac`, an HMAC of its owner, tier and instance ID, and after
  a restart it never starts, nor hands a JIT config to, a pool instance whose
  MAC is missing or wrong; it terminates it instead. The key comes from the
  new `--ec2-pool-key-file` (`FR_EC2_POOL_KEY_FILE`); `control.yaml` generates
  one in Secrets Manager and the control VM reads it at boot. Without a key,
  each process uses a random one and the warm pool is rebuilt after every
  restart. **Upgrading discards existing pool instances once:** ones launched
  by an older release carry no MAC, so they are terminated and the pool
  refills.

## [0.1.13] - 2026-11-10

### Changed

- `images/build-ec2-ami.sh` and `images/layer-ec2-ami.sh` check the build
  provenance of the firerunner release they put on an image with
  `gh attestation verify`, as `deploy-control.sh` does, and the image installs
  only a download with the SHA-256 that was checked. A `latest` build is
  pinned to the release's tag. They now need `gh`, logged in;
  `FIRERUNNER_SKIP_ATTESTATION=1` skips the check. A binary from
  `--firerunner-url` is still taken on its `--firerunner-sha256` alone, and
  `--firerunner-sha256` without `--firerunner-url` is refused.
- The control VM's bootstrap reads `<prefix>/control/env` as data: blank
  lines, `#` comments and `FR_NAME=value` lines, with a value bare or in
  quotes. Any other line, or a value holding a backslash, `$` or a backtick,
  fails the bootstrap with the line's number in the log. Values reach
  firerunner exactly as written.

### Fixed

- The control stack's JIT name cleanup removes the record only while the stack
  is being deleted (or while the create of a stack owning its zone rolls back),
  not when CloudFormation rolls back an update that added the cleanup, with the
  stack's VM still serving that name. A slow Route 53 or CloudFormation call
  now fails the delete within 5 minutes instead of an hour.
- Deleting a `control.yaml` stack no longer fails with `HostedZoneNotEmpty`.
  The control VM's JIT name record, which the VM writes itself, is now
  deleted with the stack, after the VM and before the zone; in a zone passed
  as `HostedZoneId`, only that record is removed. A stack made before this
  needs updating to the new template first, or the record deleted by hand.

### Security

- On EC2, the cache identity finds a runner's job by the runner's GitHub ID,
  from its JIT registration, as well as its name, which a runner registered in
  another scope could share. A runner whose ID is unknown is not looked up;
  JobStarted resolves it instead.
- The control VM's role may change only its own JIT name's `A` record, by
  `UPSERT`, rather than any record in the zone. `JitName` and `ZoneName` must
  be DNS names without a trailing dot.
- `--url` / `FR_URL` must be an https URL naming a GitHub organization,
  repository or enterprise; the scale-set client sends the configured
  credentials to it.
- The `build-image` workflow and the dispatched `e2e` job, which run on
  persistent self-hosted hosts, run only from `main`.

## [0.1.12] - 2026-11-10

### Changed

- A cache hit no longer rewrites cache-server's whole index under its lock:
  the entry's last-used time is written within 30 s (plus the S3 sync
  interval for an S3 index), by any other change sooner, and on shutdown. A
  lookup also scans the entries once rather than once per ref and restore key.
- cache-server lets at most 16 requests from one address, and 1024 in all,
  wait on a pending job identity at a time; one more is denied at once, with
  the new reason `waiters` in `/stats` and `/metrics`.

### Fixed

- `--log-dir-max-mib` now holds while many runners write their console logs
  at once: it counts the bytes of logs still being written, removes only
  finished logs to make room, and stops a log with a closing line when the
  budget is spent. A log still open is no longer removed.
- On EC2 runners the instance's cache proxy waited at most 30 s for a cache
  RPC, so a first restore waiting on the job's identity (up to 60 s) became a
  miss after 30 s. It now waits 90 s. Takes effect with a rebuilt runner AMI.

### Security

- The EC2 JIT endpoint now limits how many connections one source address,
  and all of them together, can hold open, closes each connection after one
  request, and uses shorter read and write deadlines, so one runner cannot
  crowd out other runners fetching their configs.

## [0.1.11] - 2026-11-10

### Fixed

- cache-server refuses, with 400 and before it claims any space, a block list
  that names a block more than once or names one never uploaded. Before, a
  repeated block could claim, and evict other cache entries for, bytes that
  were never uploaded.

### Security

- A runner denied the cache because two sources named different jobs for it
  stays denied. A lookup still running for the first job could restore its
  access when it finished.
- `deploy-control.sh` checks the build provenance of the release it deploys
  before it pins its checksums: it downloads the binaries and
  `firerunner-control-bootstrap.sh` and runs `gh attestation verify` on each,
  requiring the release workflow, the deployed tag and a GitHub-hosted runner.
  It now needs `gh`. Releases before v0.1.9 carry no attestations; set
  `DEPLOY_SKIP_ATTESTATION=1` to deploy one.
- `build-ec2-ami.sh` keeps a `--firerunner-url` out of the build's shell
  trace, and the build removes cloud-init's copies of its user-data and its
  logs before the image is taken, so a presigned URL does not end up in the
  runner image.
- `build-toolcache.sh --python` relocates Python in a container with no
  network. The pip upgrade in python-versions' `setup.sh` now reinstalls the
  pip wheel bundled in the checked archive instead of fetching one from PyPI,
  so the drive holds only what was checked.

## [0.1.10] - 2026-11-10

### Added

- `cache-server --body-idle-timeout` (default 60s) ends a request whose body
  sends nothing for that long. A steady upload is never cut.

### Changed

- `build-image` runs on manual dispatch only. Its monthly schedule waited a day
  for a `[self-hosted, kvm]` runner and was cancelled; run it, or add a
  schedule, where such a runner serves your copy.

### Fixed

- The `build-image` workflow's tool-cache job now builds a drive (Node, Go,
  Python and CodeQL by default, set with the `toolcache_tools` input); it
  passed no tools and always failed.
- The image builders write a new image beside `--out` and rename it into place
  only when it is complete, so a failed rebuild keeps the previous image instead
  of leaving a half-built one where firerunner boots from. `build-rootfs.sh`
  checks its downloads before bootstrapping, so a bad download fails in seconds.
- `build-toolcache.sh --python 3.12` picks the newest stable 3.12 release.
  It compared versions as text, so 3.12.9 beat 3.12.16, and an old release
  without a GitHub checksum then failed the build.

### Security

- The image builders believe a Node.js release's `SHASUMS256.txt` only when its
  signature checks out against a Node.js release key kept in
  `images/keys/`, and print which key signed it. Before, a checksum file
  from the same server as the tarball could not catch a compromised server.
  Building Node unpinned now needs `gpg` and `gpgv` on the build host;
  `images/keys/update-nodejs-keys.sh` refreshes the keys when a new releaser
  joins.
- `images/build-rootfs.sh` checks the `actions/runner` tarball against the
  SHA-256 in its GitHub release notes, and Node.js against the release's
  `SHASUMS256.txt`, before using them; a mismatch fails the build. It prints
  the version and checksums it used and keeps them in the image at
  `/etc/firerunner/downloads.sha256`. `--runner-sha256` and `--node-sha256`
  pin the checksums.
- `images/build-ubuntu-rootfs.sh` checks the same downloads, and Microsoft's
  `packages-microsoft-prod.deb` against a pinned SHA-256 (`--pmc-sha256`),
  on the host before the image build copies them in. `--toolset full` fetches
  actions/runner-images at a commit instead of a tag that can move: the
  default tag comes with its pinned commit, another `--runner-images-ref` is
  resolved to one and printed, and `--runner-images-commit` pins it.
- `images/build-toolcache.sh` checks Node.js against `SHASUMS256.txt`, Go
  against the go.dev download index, and Python and CodeQL against the
  SHA-256 the GitHub API reports for the release asset. `--checksums FILE`
  pins them, and each drive records its checksums in `/downloads.sha256`.
  `--go` and `--python` now need `jq`.
- `images/build-ec2-ami.sh` reads the runner's SHA-256 from its release
  notes (or `--runner-sha256`) before launching the build instance, which
  checks the download against it. runner-images is fetched at a commit:
  pinned for the default `--runner-images-ref`, resolved and printed for
  another, or given with `--runner-images-commit`. PowerShell modules are
  checked against the SHA-512 the PowerShell Gallery lists. A failed check
  fails the build, and the JSON output adds `runner_sha256`.

## [0.1.9] - 2026-11-09

### Added

- `cache-server --s3-credentials-file` signs S3 requests with the credentials
  in a file instead of the default AWS chain, and the daemon's
  `--cache-role-arn` (`FR_CACHE_ROLE_ARN`) keeps such a file fresh from a
  role it assumes, written to `--cache-credentials-file`. `status --json`
  reports the refresh as `listeners.cache_credentials`, and `doctor` fails its
  `cache-credentials` check when the file is missing or expired or the latest
  refresh failed.
- Release files carry signed build provenance: `gh attestation verify FILE
  --repo FirerunnerHQ/firerunner` checks a download came from the release
  workflow at its tag. Releases are immutable once published, and only
  repository admins can create, move or delete a `v*` tag.
- `firerunner status` reports what the guest firewall dropped since the
  network was applied, by reason: past the connection limit or rate, to a
  non-public address, or with a spoofed source. In `--json` it is
  `network.drops`, or `network.drops_error` when the counters cannot be read
  (they need root).

### Changed

- Firecracker guests now have connection limits, on by default, so one guest
  can no longer fill the host's shared conntrack table. Each guest may hold at
  most `--guest-conn-limit` connections at once (default 4096) and open at most
  `--guest-conn-rate` new ones per second (default 100, with bursts of ten
  times that). A guest's entries are removed when its VM ends. firerunner raises
  `net.netfilter.nf_conntrack_max` to fit every guest at these limits. Set
  `FR_GUEST_CONN_LIMIT=0` and `FR_GUEST_CONN_RATE=0` to turn the limits off.
  They are kept in a separate nftables table, `<table>_conns`. After a
  downgrade to an earlier version, remove that table by hand with
  `nft delete table ip firerunner_conns`.
- The cache server's errors follow the protocols it speaks. Cache RPCs answer
  each Twirp error code at the HTTP status the Twirp specification gives it (an
  oversized body is now `400 invalid_argument`, not 413), and an unknown
  service or method, a non-POST request or a body not sent as JSON is a
  `404 bad_route` in JSON, where Go's plain-text 404 or 405 used to make the
  toolkit retry. Blob uploads and downloads fail as Azure Storage does, with
  the code in `x-ms-error-code` and an XML `Error` body, including
  `416 InvalidRange` and `405 UnsupportedHttpVerb`; a full cache and a store
  outage stay `507`, which clients do not retry. Errors no longer carry host
  paths, and the server's own log names the entry.
- `firerunner status` and `doctor` read the daemon's settings from
  `/etc/firerunner/firerunner.env` when no `FR_*` variable is set, and say so,
  so run from a shell on the host they describe the running deployment rather
  than a default one. `--env-file FILE` reads another file, `--env-file none`
  none.

### Fixed

- The cross-account role in `deploy/cloudformation/byoc-role.yaml` can no
  longer read `<prefix>/control/*`, where a control VM in the same account
  keeps its GitHub credentials. Update the stack to apply it.
- Firecracker console logs under `--log-dir` are capped: each keeps at most
  `--log-max-mib` (default 64), and all of them at most `--log-dir-max-mib`
  (default 2048), the oldest removed first. They are created mode 0600.
- A runner still running two minutes after GitHub reports its job completed
  is stopped, so a guest that disables its own shutdown no longer holds its
  slot or instance until `--max-vm-lifetime`.
- The cache server limits request bodies: 64 KiB for cache RPCs and 8 MiB for
  a block list. A larger body gets `413`. A block list may name at most
  50,000 blocks, and a block id at most 88 characters, as in Azure.
- The cache server counts uploads that are not yet finalized toward
  `--max-size`, so its disk use stays within the cap. An upload that would go
  past it while other uploads are unfinished gets `507 Insufficient Storage`.
- The cache server bounds its number of entries, not only their bytes: a
  repository keeps at most `--max-repo-entries` (default 2000), evicting its
  own least-recently-used first, and the host at most `--max-entries` (default
  20000). A job may hold at most 64 unfinished uploads, and requests with more
  than ten keys or a key longer than 512 characters are refused, as
  `@actions/cache` does.
- Cache entries are scoped by GitHub's repository ID as well as its name, so a
  repository created later under a name another one gave up reads none of the
  earlier one's caches. Entries saved before this version are no longer
  restored, and age out under the size and entry caps; the first jobs after an
  upgrade miss the cache once, as do a repository's first jobs after it is
  renamed or transferred. Upgrade the daemon and cache-server together.
- Firecracker guests no longer reach private addresses through a host's own
  DNAT rules, such as Docker's or Kubernetes' published ports, in any egress
  mode. Only the host's redirect of guest DNS (TCP or UDP to port 53) is still
  exempt from the non-public drop, and packets conntrack marks invalid are
  dropped.
- `git_cache` saves only the job's own repository. A fetch of another one, such
  as a second checkout or a submodule, was stored under the job's repository
  and default branch, where a fork PR could restore it. Git cache keys now
  name their repository; entries saved before this version are never restored,
  to the proxy or any other client. Images built before this version keep
  fetching from GitHub but no longer save to the git cache; rebuild them to
  get it back.

### Security

- With the control stack's `Cache=s3`, cache-server no longer uses the control
  VM's instance role. The stack adds a cache role holding only the bucket's
  prefix, which the daemon assumes and hands to cache-server in a file;
  cache-server cannot reach instance metadata, as with `Cache=disk`, and the
  control role loses its S3 access. Deploy this release before updating the
  stack, then refresh the VM.

## [0.1.8] - 2026-11-07

### Added

- `FR_APP_KMS_KEY` signs the GitHub App's JWTs with a key kept in AWS KMS, so
  the daemon never holds it. The control VM stack takes its ARN as
  `AppKMSKeyArn`.

### Changed

- firerunner uses actions/scaleset v0.5.0. A message from GitHub is now deleted
  only after it is acted on, so a crash in between has it delivered again rather
  than lost.

## [0.1.7] - 2026-11-07

### Added

- `FR_TENANTS` serves more installations from one daemon, on its GitHub App and
  tier catalog, with caches off and no hot runners. This is groundwork for the hosted service.
- The `job_timeline` view has `supply_seconds`, from GitHub assigning a job to the
  job starting. `queue_seconds` stays empty for now, as GitHub's scale set messages
  carry no queue time.

## [0.1.6] - 2026-11-06

### Added

- `FR_JOB_EVENTS_DSN` keeps each job's stages, machine and result in Postgres. It
  is part of `proprietary/`, for the hosted service, and is off unless set. The
  release binaries now include that code, under its own license.
- `images/layer-ec2-ami.sh`, which puts a firerunner release on an existing AMI.
  It took 84 s, against about 40 minutes for a full build.

### Changed

- `build-ec2-ami.sh` keeps its scratch files out of the snapshot.

## [0.1.5] - 2026-11-06

### Fixed

- A launch no longer waits about 5 s on each option with no spot capacity, which
  took a job's queue time to about 50 s when the first types had none.
- A cache hit returns the key as the job wrote it, so `setup-node` no longer saves
  again and warns on every run.

## [0.1.4] - 2026-11-06

### Changed

- A job's first `docker` command no longer waits on Docker's start, with an image
  from this release: about 0.1 s instead of 1.8 s on r8a.xlarge. Cold-launch
  queue time fell by about 1.5 s with it.

## [0.1.3] - 2026-11-06

### Changed

- EC2 instances reach the runner sooner, with an image from this release. The
  time from the config being served to the runner listening fell from about
  10 s to about 3.5 s on r8a.xlarge. Docker and the git cache's proxy now start
  alongside the runner, so a job's first `docker` command may wait several
  seconds for it.
- The image no longer starts PHP-FPM, Podman's services and other services a job
  does not use at boot. A job can still start them.
- The standard toolset no longer includes the `Microsoft.Graph` PowerShell module.

## [0.1.2] - 2026-11-06

### Changed

- `docker_storage: memory` keeps Docker's data on tmpfs on an instance with about
  32 GiB of memory or more, with an image from this release. Compression then
  waits for memory to run short, and a large image build took 304 s on
  r8a.xlarge. Smaller instances keep compressed RAM.

## [0.1.1] - 2026-11-06

### Added

- `git_cache` on an EC2 tier, which serves a fresh checkout's fetch through
  cache-server. A repeat fetch of a 200 MB checkout took 3-6 s instead of 20 s.
  A commit's first fetch still comes from GitHub, so retries and later jobs on
  the same commit gain, not the first. It needs `cache`, and an image built from
  this release.
- `firerunner git-proxy`, the proxy on the instance that serves it.
- `firerunner doctor` checks that a GitHub App installed on selected
  repositories covers what a cache serves: it fails when a runner's repository
  is missing, and lists the repositories, with a warning, for an organization.

### Changed

- The log now says why a job has no cache when GitHub refuses a token for its
  repository, and asks whether the App is installed on it.

## [0.1.0] - 2026-11-05

First release. The prereleases `v0.1.0-rc.1` to `v0.1.0-rc.12` are not listed
separately.

### Added

#### Runners

- Ephemeral GitHub Actions runners, one fresh machine per job, driven by
  GitHub's runner scale-set long-poll.
- Firecracker microVM runners, with per-VM networking, an nftables egress
  allowlist, off-VM logs and the optional jailer.
- EC2 runners, per tier through `"provisioner": "ec2"`: one instance per job, on
  spot, on demand, or spot falling back to on demand.
- A tier catalog, so `runs-on` selects a size, with a warm pool, hot runners
  sized by schedule, and a stopped EC2 pool with `pool_for`.
- A JIT config that never enters EC2 user-data: it is served once, over the
  VPC, to the instance's own address.

#### EC2

- Root volume settings: `root_volume_gib`, `root_volume_type`,
  `root_volume_throughput`, `root_volume_iops` and `root_volume_init_rate`.
- `docker_storage: memory`, which keeps Docker's data in compressed RAM. It is
  much faster for builds that fit, and a build that outgrows memory fails, so a
  tier opts in.
- A reproducible AMI build, `images/build-ec2-ami.sh`, with `standard` (the
  default), `full` and `minimal` toolsets based on GitHub's runner images.
- Boot tuning for a disk that lasts one job: the root filesystem grows to the
  volume, zram swap, and faster `apt` and `dpkg`.
- Retries of stalled SYNs on a new source port for two minutes after boot, for
  connections a new instance in us-east-1 can lose for up to 38 s.
- Fallback past vCPU quotas and refused launch options, and reaping of orphaned
  instances and spot requests.
- CloudFormation templates for the network and the control VM, with a reaper
  and a cross-account role.

#### Cache

- `firerunner cache-server`, a self-hosted `actions/cache` backend for
  Firecracker and EC2 tiers, on disk or S3, scoped by host-authenticated job
  identity.

#### Commands

- `firerunner status` and `firerunner doctor`, with `--json`.
- `firerunner config check`, to validate a configuration or tier catalog offline.
- `firerunner migrate`, which opens a pull request moving a repository's jobs
  onto firerunner.

[Unreleased]: https://github.com/FirerunnerHQ/firerunner/compare/v0.1.22...HEAD
[0.1.22]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.22
[0.1.21]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.21
[0.1.20]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.20
[0.1.19]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.19
[0.1.18]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.18
[0.1.17]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.17
[0.1.16]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.16
[0.1.15]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.15
[0.1.14]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.14
[0.1.13]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.13
[0.1.12]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.12
[0.1.11]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.11
[0.1.10]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.10
[0.1.9]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.9
[0.1.8]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.8
[0.1.7]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.7
[0.1.6]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.6
[0.1.5]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.5
[0.1.4]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.4
[0.1.3]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.3
[0.1.2]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.2
[0.1.1]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.1
[0.1.0]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.0
