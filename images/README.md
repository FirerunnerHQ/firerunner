# firerunner golden images

firerunner boots each job in a fresh microVM cloned (reflink) from an immutable
**golden rootfs**. The image bakes in the official `actions/runner` agent plus a
tiny boot service that reads its JIT config from MMDS and starts the runner for
exactly one job. This directory holds the (host-side) build tooling and the
rebuild pipeline.

> These scripts run on a Linux KVM host — they cannot build on macOS. Treat this
> as the reference/scaffold; run it on `starship` or the self-hosted `kvm`
> runner.

## Why a golden image (not Docker)

firerunner deliberately avoids OCI/containerd. The rootfs is a plain ext4 file:
no image registry, no layer graph, no container runtime on the critical path.
Cloning is a filesystem reflink (btrfs/XFS), so a new job's disk appears in
milliseconds and is thrown away after the run.

## Tiers

Different workloads need different images. Each tier is a separate golden rootfs
selected by the runner label a workflow requests (`runs-on: <tier>`). In the
scale-set model GitHub routes a job to the scale set whose name matches that
label; one firerunner process serves every tier (each registered as its own
scale set) from a single [tier catalog](../examples/tiers.json).

| Tier                      | vCPU / RAM (suggested) | Contents                                              | Use for                                                     |
| ------------------------- | ---------------------- | ---------------------------------------------------- | ----------------------------------------------------------- |
| `firerunner-4c8g`         | 4 / 8 GiB              | actions/runner, git, curl, jq, build-essential base   | generic jobs; toolchains fetched by `setup-*` per workflow  |
| `firerunner-node`         | 2 / 4 GiB              | everything above **+ Node.js LTS + npm** (baked)      | JS/TS builds where `setup-node` should hit a local toolchain |
| `firerunner-8c16g-docker` | 8 / 16 GiB            | everything above **+ Docker Engine (dind-capable)**  | jobs using `container:`, service containers, `docker build` |
| `firerunner-ubuntu-min`   | 4 / 8 GiB              | Ubuntu 24.04 (glibc 2.39) thin base: runner, git, build-essential, system python3, Node.js runtime — **no Docker, no baked language toolchains** | ubuntu-latest glibc parity for the ~90% of jobs that don't need Docker; pair with a `--toolcache` drive |
| `firerunner-ubuntu`       | 4 / 8 GiB              | Ubuntu 24.04 (glibc 2.39) **+ hosted tool cache + runner-images installers** (kitchen-sink) | closest parity with GitHub-hosted `ubuntu-latest` |

The first three tiers are lean Debian goldens built by
[`build-rootfs.sh`](build-rootfs.sh); the two `firerunner-ubuntu*` tiers are
built by [`build-ubuntu-rootfs.sh`](build-ubuntu-rootfs.sh), which offers three
Ubuntu toolsets on the same glibc-2.39 parity base:

- `--toolset minimal` — thin base (runner + git + build-essential + system
  python3 + Node.js runtime). **No Docker, no baked jdk/ruby/go.** Languages come
  from an attached [`--toolcache` drive](#building-the-tool-cache-drive) via
  `setup-*` actions, so this is the smallest Ubuntu-parity golden (~1.6 GiB) for
  the common case of build/test/lint/SAST jobs that never touch Docker.
- `--toolset base` — `minimal` + `docker.io` + the curated apt kitchen-sink
  (`default-jdk`, `ruby-full`, `golang-go`, …); self-contained, `ubuntu-latest`-ish.
- `--toolset full` — `base` + the actions/runner-images tool cache and curated
  docker-safe installer subset (Azure/GUI/snap/services excluded); maximum parity.

Every toolset (and every Debian tier) ships `zstd`, as `ubuntu-latest` does.
Without it `actions/cache` falls back to single-threaded gzip, which on a
2-vCPU guest dominates the restore time of a large entry. The compression
method is part of the cache entry version, so the first job on a golden that
gains zstd misses every entry an older gzip guest saved, once per key.

The base tier deliberately ships no language toolchain — a workflow's `setup-go`
/ `setup-node` fetches one per run. Baking a toolchain (the `firerunner-node`
tier) removes that download, mirroring GitHub's hosted **tool cache**
(`/opt/hostedtoolcache`). The Docker tier exists because some pipelines rely on
`docker build`, job `container:` and service containers, which need a real
Docker daemon inside the guest.

### Running several tiers on one host

Serve every tier from **one** firerunner process: list them in a JSON tier
catalog and point `--tiers` (or `FR_TIERS`) at it (see
[`examples/tiers.json`](../examples/tiers.json) and the "Runner tiers" section
of the top-level [README](../README.md)). All tiers then share one host
network, tool cache and the `--max-runners` slot budget — `vcpu`/`mem_mib`/
`golden` vary per tier — and developers pick one with `runs-on: <name>`. No
per-instance network identity or extra systemd units are needed.

## What the boot service does (MMDS → JIT)

At boot the guest fetches its runner JIT config from Firecracker MMDS v2. This
is implemented by [`assets/firerunner-run.sh`](assets/firerunner-run.sh), started
by [`assets/firerunner-runner.service`](assets/firerunner-runner.service):

1. Grab an MMDS token: `PUT http://169.254.169.254/latest/api/token`
   (`X-metadata-token-ttl-seconds`).
2. `GET http://169.254.169.254/jitconfig` with `X-metadata-token` → the base64
   JIT config firerunner published (from `{"jitconfig": "<base64>"}`).
3. Run `./run.sh --jitconfig <base64>` (ephemeral; auto-deregisters after one
   job). The runner runs as root (`RUNNER_ALLOW_RUNASROOT`) — the whole microVM
   is disposable, and it lets the boot service issue the final reboot.
4. On exit the boot service issues `reboot -f`; with `reboot=k` on the kernel
   cmdline this makes the VMM exit and the host reaps the microVM.

A static `/etc/resolv.conf` (matching `--dns-servers`) is baked in because the
egress allowlist only permits the configured resolvers.

### EC2 runner instances

An EC2 runner AMI runs [`assets/firerunner-ec2-run.sh`](assets/firerunner-ec2-run.sh)
from [`assets/firerunner-ec2-runner.service`](assets/firerunner-ec2-runner.service)
instead. The JIT config never appears in user-data:

1. Read user-data through IMDSv2. It holds one line,
   `FIRERUNNER_JIT_URL=http://<control-vm-vpc-ip>:<port>/jit` (firerunner's
   `--jit-addr`). With no such line, for example during a warm-up or image-build
   boot, the instance powers off.
2. `GET` that URL until it answers. firerunner serves the config **once**, to
   this instance's private IP, then answers 404.
   - Connection errors and 403 mean the address is not registered yet, so the
     script retries.
   - A 404 means the config was claimed or expired, so the instance powers off.
   - It gives up after `FIRERUNNER_JIT_WAIT` seconds (default 600).
3. Run `./run.sh --jitconfig <config>` for one job, then power off. The
   instance is launched to terminate on shutdown. A pooled persistent-spot
   instance, which can only stop, is terminated by firerunner.

The unit has no `network-online.target` or docker ordering: the retry loop
starts the fetch as soon as the NIC has an address. `images/test-ec2-run.sh`
exercises the script against a fake IMDS and endpoint.

### Building the EC2 runner AMI

`images/build-ec2-ami.sh` builds the image from stock Ubuntu 24.04 and
publishes its ID to an SSM parameter. Point a tier's `"ami"` at that
parameter:

```bash
images/build-ec2-ami.sh \
  --subnet subnet-0aaa… --security-group sg-0bbb… \
  --ssm-param /firerunner/ami/runner --tag team=ci
# {"ami":"ami-…","ssm_param":"/firerunner/ami/runner","runner_version":"2.338.1","arch":"x64"}
```

It launches a build instance with `images/ec2/provision.sh` as user-data,
waits for it to power off, snapshots it, publishes the ID and terminates the
build instance on every exit path. A build takes about 5 minutes.

- The build instance needs egress to Ubuntu's mirrors and github.com, but no
  inbound rules and no instance profile.
- `--render` prints the user-data without touching AWS.
- `images/test-build-ec2-ami.sh` tests the script against a fake `aws`.

The image contains:

- docker with the containerd image store;
- `actions/runner`, `zstd` and the boot unit above.

It leaves out cloud-init, snapd, and timers that could wake during a job.
Two settings matter for any image made from a snapshot:

- **netplan matches any NIC by name.** cloud-init's `50-cloud-init.yaml` matches
  the build instance's MAC address. With cloud-init disabled, every instance
  from the image would boot without network.
- **The hostname and machine-id are emptied,** so each instance takes its own
  from DHCP and at first boot, not the build instance's.

Keep the image small. Do not bake CI container images into it: EBS restores
snapshot blocks lazily, so a large image makes first reads, and jobs, slower
than pulling from the registry.

**Rebuild at least monthly.** GitHub accepts a runner version for about 30
days after its release. A tier that names the SSM parameter picks up a new
image within five minutes, with no config change.

## Building

```bash
# On a Linux KVM host, as root, with debootstrap installed:
sudo ./build-rootfs.sh \
  --tier firerunner-4c8g \
  --runner-version 2.320.0 \
  --out /var/lib/firerunner/golden-4c8g.ext4

# Node tier (bakes Node.js LTS); --node-version overrides the default:
sudo ./build-rootfs.sh \
  --tier firerunner-node \
  --node-version 22.11.0 \
  --out /var/lib/firerunner/golden-node.ext4
```

`build-rootfs.sh` creates/formats the ext4 image, debootstraps a minimal Debian
base (systemd + git/curl/iproute2), installs `actions/runner` and runs its
`installdependencies.sh`, drops in the boot service + a static resolv.conf,
enables the service, and (per tier) installs Docker or bakes Node.js. The result
is an immutable file firerunner reflink-clones per job. Requires `debootstrap`,
`mkfs.ext4`, `curl` and `tar` on the host.

## Building the tool-cache drive

`firerunner --toolcache` / `FR_TOOLCACHE` attaches a **separate**, read-only
`hostedtoolcache`-labelled ext4 to every microVM (mounted at
`/opt/hostedtoolcache` as an overlay lower layer under a per-VM tmpfs upper),
so `setup-go` / `setup-node` / `setup-python` find their toolchain on disk
instead of downloading it per job — while still being able to install an
uncached version into the writable tmpfs upper instead of hitting EROFS on the
read-only drive. Unlike the baked-in cache
of the `firerunner-ubuntu` full golden, this drive is **decoupled from the OS
image**: an operator can re-cut it with new versions or more tools without
rebuilding a rootfs, and one drive is shared by every tier.

[`build-toolcache.sh`](build-toolcache.sh) is the builder. Pick exactly the
tools + versions your team uses:

```bash
# Node + Go: official tarballs, no Docker needed; multiple versions per tool.
sudo ./build-toolcache.sh --out /var/lib/firerunner/toolcache.ext4 \
  --node 20.18.0,22.22.2 --go 1.27.0

# Python: fetched from actions/python-versions and relocated inside an
# ubuntu:24.04 container (its interpreter bakes an absolute RUNPATH under
# /opt/hostedtoolcache, matching where the guest mounts the drive) => needs Docker.
sudo ./build-toolcache.sh --out toolcache.ext4 --python 3.12

# CodeQL: bakes the github/codeql-action bundle exactly as runner-images does
# (self-contained, no Docker; needs jq). `latest` tracks the version the newest
# codeql-action pins; pass a CLI version (e.g. 2.26.3) to pin it.
sudo ./build-toolcache.sh --out toolcache.ext4 --codeql latest
```

It lays out the exact hosted-tool-cache structure the stock actions look for
(`<tool>/<version>/x64/` + a `<version>/x64.complete` marker; CodeQL also gets a
`pinned-version` file so the Action uses the baked bundle authoritatively instead
of re-downloading when its default bumps), sizes and formats the ext4, and labels
it `hostedtoolcache`. The drive is a **pure accelerator**: a missing tool/version
just means `setup-*` downloads it and the job still passes. Tailor the version
list to the repos you serve by reading their `go.mod` / `.nvmrc` /
`.python-version`.

## Rebuild policy

GitHub only supports self-hosted runner agents released within the **last 30
days**. `.github/workflows/build-image.yml` rebuilds **every** shipped image on a
schedule (monthly, well within the window) and on manual dispatch, pinning the
latest `actions/runner` release. Its matrix covers all five rootfs tiers —
`golden` (shared by `firerunner` / `firerunner-8c16g`), `golden-docker`,
`golden-node`, and the two `ubuntu-rootfs-*` goldens — so none falls out of the
30-day window. The same run also rebuilds the [`--toolcache` drive](#building-the-tool-cache-drive):
it bakes no runner agent (so it is exempt from the 30-day rule) but is refreshed
on the same cadence to pick up toolchain and base-OS updates. Rebuilding the
rootfs images also folds in OS security updates so every microVM starts patched.
The publish directory defaults to `/var/lib/firerunner` and can be overridden via
the `image_dir` dispatch input.
