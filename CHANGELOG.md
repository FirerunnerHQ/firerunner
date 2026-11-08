# Changelog

All notable changes to firerunner are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

### Fixed

- The cross-account role in `deploy/cloudformation/byoc-role.yaml` can no
  longer read `<prefix>/control/*`, where a control VM in the same account
  keeps its GitHub credentials. Update the stack to apply it.
- The cache server limits request bodies: 64 KiB for cache RPCs and 8 MiB for
  a block list. A larger body gets `413`. A block list may name at most
  50,000 blocks, and a block id at most 88 characters, as in Azure.
- The cache server counts uploads that are not yet finalized toward
  `--max-size`, so its disk use stays within the cap. An upload that would go
  past it while other uploads are unfinished gets `507 Insufficient Storage`.

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

[Unreleased]: https://github.com/FirerunnerHQ/firerunner/compare/v0.1.8...HEAD
[0.1.8]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.8
[0.1.7]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.7
[0.1.6]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.6
[0.1.5]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.5
[0.1.4]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.4
[0.1.3]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.3
[0.1.2]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.2
[0.1.1]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.1
[0.1.0]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.0
