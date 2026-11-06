# Changelog

All notable changes to firerunner are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/FirerunnerHQ/firerunner/compare/v0.1.3...HEAD
[0.1.3]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.3
[0.1.2]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.2
[0.1.1]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.1
[0.1.0]: https://github.com/FirerunnerHQ/firerunner/releases/tag/v0.1.0
