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

1. Read user-data through IMDSv2. It holds
   `FIRERUNNER_JIT_URL=http://<control-vm-vpc-ip>:<port>/jit` (firerunner's
   `--jit-addr`), and `FIRERUNNER_DOCKER_STORAGE` for a tier that sets
   `docker_storage`. With no such URL, for example during a warm-up or image-build
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
4. Report progress with `POST <url>/report?phase=…`: `listening` and `job`
   when the runner prints "Listening for Jobs" and "Running job:", and
   `exited&rc=<status>` before powering off. The status is the listener's:
   upstream's `run.sh` keeps the listener alive across retries and
   self-updates but exits 0 for most fatal statuses, so the script reads the
   status from the line `run-helper.sh` prints for it (terminated 1, session
   conflict 5, unknown N), and waits (bounded, `FIRERUNNER_DRAIN_TICKS`) for
   the output reader to reach `run.sh`'s last line, so the phase reports go
   out before the exit's. A missing or non-executable
   `run.sh` reports status 127. Any other exit after the config arrived (an
   empty config, a failed setup step) reports a failure, as does a 404 after a
   fetch whose response was lost. A signal (shutdown, spot interruption,
   terminate) reports nothing and leaves the poweroff to whoever sent it: the
   instance going away is not the runner failing. Reports are best effort (2 s
   each, `FIRERUNNER_REPORT_TIMEOUT`). The runner's output reaches the reader
   through a FIFO, so the script waits for the runner alone, not for a process
   a job left holding its output. A broken journal stream costs log lines,
   not the job or its report: the unit runs the script with SIGPIPE ignored
   (`IgnoreSIGPIPE=yes`, systemd's default), the reader drops a line it cannot
   write, and the script reports before it logs.

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
build instance on every exit path.

- The build instance needs no inbound rules and no instance profile. A minimal
  build needs egress to Ubuntu's mirrors and github.com. The other toolsets
  need the wider internet, because GitHub's installers fetch from each tool's
  own source.
- `--render` prints the user-data without touching AWS.
- `images/test-build-ec2-ami.sh` tests the script against a fake `aws`.
- `images/test-ec2-vm.sh` tests the image with no AWS. On a Linux host with
  KVM, it builds the image from Ubuntu's cloud image in a VM, boots it on a
  larger disk against a fake IMDS and JIT endpoint, and checks what a job sees.

`--toolset` picks what the image holds:

| toolset | contents | disk | build |
|---|---|---|---|
| `standard` (default) | firerunner's choice: the tools of GitHub's `ubuntu-24.04` that jobs commonly use. Jobs run as `runner`, with sudo | 18 GiB | about 20 minutes |
| `full` | nearly all of them, for the closest match with `ubuntu-latest` | 44 GiB | about 30 minutes |
| `minimal` | the runner, Docker, git. Jobs run as root. The default on arm64 | 3 GiB | about 5 minutes |

`standard` and `full` run the installers of an
[actions/runner-images](https://github.com/actions/runner-images) release, in
the order its Packer template lists them, so a new release needs only
`--runner-images-ref`. Each toolset picks a build instance, volume and timeout
that fit it, which `--instance-type`, `--volume-gib` and `--timeout` override.

`standard` keeps the latest version of most tools, plus defaults that system
paths link to, such as Go's. `setup-*` actions fetch other versions. Language
stacks few jobs use are left out, as are Android, Homebrew and preloaded Docker
images. `images/test-runner-images.sh` tests the trimming.

Because the disk lasts for one job, the image favours speed over durability:

- `firerunner-syn-reroll` retries a stalled SYN to a public address on a new
  source port for the first two minutes, because a new instance in us-east-1
  can lose an outbound connection's SYNs for 1-38 s. See
  [`assets/firerunner-syn-reroll.nft`](assets/firerunner-syn-reroll.nft).
- For a tier with `git_cache`, `firerunner-git-proxy.service` answers fresh
  fetches from cache-server. git is pointed at it only while it runs, because
  git treats a dead proxy as a failed fetch. The image takes the binary from a
  release, checked against `SHA256SUMS`, with `--firerunner-version` choosing
  which, or from `--firerunner-url` with `--firerunner-sha256`. A binary that
  fails the check, or has no `git-proxy`, stays out, and the image then has no
  git cache.
- `firerunner-ec2-tune` grows the root filesystem to the volume's size on each
  job boot, so `root_volume_gib` takes effect, adds zram swap as large as
  memory, moves Docker's data to RAM for a tier with
  `docker_storage: memory`, on tmpfs from about 32 GiB and in zram below that, and reads Docker's and the runner's files in the
  background. Each step is best effort, and the job starts either way. Docker
  starts here too, once its data is in place, and the runner does not wait for
  it: the first `docker` command does, through the socket.
- The root filesystem mounts with `data=writeback`, `journal_async_commit` and
  `nobarrier`. A pool instance stops cleanly, so these never risk a disk that
  boots again.
- Transparent huge pages are always on, `dpkg` runs under `eatmydata` and `apt`
  fetches gzip indexes. The last two follow
  [runner-images-for-aws](https://github.com/runs-on/runner-images-for-aws).
- The boot service keeps `OOMPolicy=continue`: a job's process killed for
  memory fails its step, and must not stop the service that powers off.

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

## Verified downloads

Every archive a builder runs or bakes into an image is checked before it is
used, against a SHA-256 that is pinned or that comes from a source other than
the download itself. A mismatch stops the build with an error that names the
file and both checksums, and nothing from the file is unpacked. Each build
prints the checksums it used, and a rootfs keeps them in
`/etc/firerunner/downloads.sha256`.

| download | checked against | pin it with |
|---|---|---|
| `actions/runner` (`build-rootfs.sh`, `build-ubuntu-rootfs.sh`) | the SHA-256 in the release's notes, read through the GitHub API | `--runner-version` and `--runner-sha256` |
| Node.js (`build-rootfs.sh` node tier, `build-ubuntu-rootfs.sh`) | `SHASUMS256.txt` in the same nodejs.org release directory | `--node-sha256` |
| actions/runner-images (`build-ubuntu-rootfs.sh --toolset full`) | a commit SHA, fetched with git | `--runner-images-commit` |
| `packages-microsoft-prod.deb` (`build-ubuntu-rootfs.sh --toolset full`) | a SHA-256 pinned in the script for Ubuntu 24.04 | `--pmc-sha256` |

An empty `--runner-version` resolves the latest release once, and the build
prints the version and checksum it took. A pinned checksum skips the lookup.
`build-ubuntu-rootfs.sh` downloads and checks everything on the host, and its
Dockerfile only copies the checked files in.

A runner-images tag can move, so the build fetches a commit. The default
`--runner-images-ref` comes with the commit it named when it was pinned; bump
both together, taking the commit from
`gh api repos/actions/runner-images/commits/TAG --jq .sha`. Any other
`--runner-images-ref` is resolved to a commit once, through the GitHub API,
and the build prints it so `--runner-images-commit` can pin it. git checks
every object it fetches against its hash.

To pin `packages-microsoft-prod.deb` for another Ubuntu release, download it,
check that the key it installs has the fingerprint
`BC52 8686 B50D 79E3 39D3 721C EB3E 94AD BE12 29CF`, and pass its `sha256sum`
as `--pmc-sha256`, or add it to the script's table.
`SHASUMS256.txt` comes from the same origin as the tarball, and its GPG
signature is not checked; for a stronger guarantee, pin `--node-sha256` from a
`SHASUMS256.txt.asc` you verified.

`images/test-verify-downloads.sh` runs the builders against a fake network
that serves tampered archives.

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
