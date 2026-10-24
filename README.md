# firerunner

[![CI](https://github.com/FirerunnerHQ/firerunner/actions/workflows/ci.yml/badge.svg)](https://github.com/FirerunnerHQ/firerunner/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/firerunnerhq/firerunner.svg)](https://pkg.go.dev/github.com/firerunnerhq/firerunner)
[![Go Report Card](https://goreportcard.com/badge/github.com/firerunnerhq/firerunner)](https://goreportcard.com/report/github.com/firerunnerhq/firerunner)
[![Go version](https://img.shields.io/github/go-mod/go-version/FirerunnerHQ/firerunner)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)

Ephemeral [Firecracker](https://firecracker-microvm.github.io/) microVM runners
for GitHub Actions. Every job runs in a fresh, single-use microVM that registers
a just-in-time (JIT) ephemeral runner, executes exactly one job, and then self-
destructs.

Built for **maximum control and a minimal dependency surface**: firerunner talks
to the Firecracker REST API directly over its unix socket using only the Go
standard library — no containerd, no CNI, no LVM, no VM-management daemon. Its
only external dependency is GitHub's official runner scale-set client
(`github.com/actions/scaleset`), which drives the long-poll control plane.

> Status: early, but validated end-to-end on real hardware. The full path —
> GitHub scale-set long-poll, per-VM networking, the nftables egress allowlist,
> off-VM log shipping, golden-image build, and microVM boot → MMDS-JIT →
> self-destruct — has been exercised on a KVM host (Firecracker v1.10.1).
> Running a live production fleet additionally needs a golden image and a GitHub
> App (see below).

## Why microVMs

Self-hosted runners execute untrusted third-party code (dependencies, actions,
fork PRs). Containers share the host kernel, so a container escape reaches the
host — unacceptable when the host holds credentials. A Firecracker microVM has
its own kernel and hardware-enforced (KVM) isolation, at ~1–2% overhead versus
bare metal. See GitHub's guidance on
[self-hosted runner security](https://docs.github.com/en/actions/reference/runners/self-hosted-runners).

## Architecture

```
                 ┌──────────────────── firerunner (single Go binary) ─────────────────┐
GitHub  ◀──────▶ │  listener        long-poll GitHub → desired runner count           │
(scaleset API)   │  scheduler       reconcile running microVMs to desired, ≤ maxRunners│
                 │  provisioner     per job: reflink golden.ext4 → tap+nft → MMDS(JIT) │
                 │                  → firecracker InstanceStart → wait exit → reap      │
                 └────────────────────────────────────────────────────────────────────┘
                                              │  one microVM per job
                                              ▼
                        ┌──────── Firecracker microVM (ephemeral) ────────┐
                        │  actions/runner --jitconfig  → run ONE job      │
                        │  then `reboot -f` → VMM exits → host reaps it    │
                        └─────────────────────────────────────────────────┘
```

The guest self-destructs with `reboot -f` (not `poweroff`): with `reboot=k` on
the kernel command line this triggers an i8042 reset that makes the Firecracker
VMM process exit, which the host detects to reap the job.

### Networking

Each microVM gets its own tap device on a dedicated `/30` subnet
(`172.16.<slot>.0/30`; host gateway `.1`, guest `.2`), so many VMs run in
parallel without collisions. The guest address, gateway and netmask are handed
to the kernel via the `ip=` boot argument (no DHCP needed). Slot allocation is
bounded by `--max-runners`.

Egress is controlled by an **nftables allowlist** (in a dedicated `firerunner`
table). Instead of a blanket masquerade, the host enables IPv4 forwarding and
only forwards guest traffic destined for GitHub's own IP ranges — fetched from
[`api.github.com/meta`](https://docs.github.com/rest/meta/meta#get-github-meta-information)
and refreshed periodically (`--meta-refresh`, default 24h) — plus optional DNS
and NTP. Everything else from `172.16.0.0/16` is dropped, then the allowed
traffic is masqueraded out the external interface (`--ext-iface`). Configure the
allowlist with `--egress` (default `api,actions,git,dns,packages,ntp`); the
GitHub categories are `api`, `actions`, `git`, `packages`, and `dns`/`ntp` are
pseudo-categories. Pass `--egress open` to disable the allowlist and fall back to
blanket NAT.

In **every** mode, `--egress open` included, guests are kept off non-public
networks and mail: the forward chain drops guest traffic to any non-globally-
reachable IPv4 range (RFC1918 — so the host's LAN — the `100.64.0.0/10` tailnet
range, link-local `169.254.0.0/16` metadata, loopback, documentation, multicast, reserved) and
to TCP ports 25, 465, 587 and 2525. Two exceptions, both chosen by the host
rather than the guest: a resolver listed in `--dns-servers` stays reachable on
port 53 in allowlist mode even on the LAN, and a destination the host itself
rewrote with DNAT (for example, redirecting guest DNS to a local resolver) is
not treated as a guest-chosen private address. `make netns-test` proves this
against the real kernel in throwaway network namespaces.

The same table also protects the **host itself**: an `input` chain drops
guest→host traffic (so a guest cannot reach host-local services), allowing only
the optional cache-server port on its gateway; interface-keyed **anti-spoof**
drops any guest packet whose source is outside the VM range; and a companion
`ip6` table black-holes all guest-originated IPv6 (guests are IPv4-only). All of
these are scoped to firerunner's own tap/veth interfaces, so the host's other
traffic is untouched. This protection applies even in `--egress open` mode.

### Logs

Because the microVM is destroyed after its one job, its serial console (which
carries the runner/job output) is streamed to a per-runner file under
`--log-dir` on the host — off-VM log forwarding, as GitHub recommends for
ephemeral runners.

## Alignment with GitHub's official recommendations

- **Ephemeral / JIT runners** — one job per runner, auto-deregistered (GitHub's
  recommended model for autoscaling).
- **Official runner agent** — uses `actions/runner` inside the golden image.
- **Official scaling API** — built on `github.com/actions/scaleset` (reliable
  long-poll; GitHub warns webhook-based scaling is less reliable).
- **Least-privilege auth** — GitHub App preferred over PAT.
- **Clean environment per job** — reflink-cloned rootfs, destroyed after use.
- **Per-VM network isolation + egress allowlist** — dedicated tap/subnet per
  microVM; guests may reach only GitHub's published IP ranges (plus DNS/NTP).
- **External log forwarding** — serial console shipped off-VM to `--log-dir`.
- **Golden-image rebuild pipeline** — images rebuilt on a schedule so the
  bundled `actions/runner` stays within GitHub's ≤30-day support window (see
  [`images/`](images/README.md)).

## What a runner can see

While a job runs, its VM holds what the job was given:

- the checked-out source;
- the secrets the workflow references;
- the job's `GITHUB_TOKEN`, scoped by the workflow's `permissions:`;
- any cloud credentials it obtains through OIDC.

That holds for every self-hosted or hosted runner. firerunner limits how far
that reaches and how long it lasts:

- **One VM per job.** A Firecracker microVM's rootfs is a per-job clone, and
  it is deleted when the job ends. An EC2 runner is terminated after its job,
  along with its disk.
- **Nothing reaches in.** The runners' security group has no inbound rules.
  A Firecracker guest has its own tap device and subnet, and it can't reach
  the host's private networks.
- **No cloud identity.** EC2 runners get no instance role. IMDSv2 is
  required with a hop limit of 1, so job containers cannot reach it.
- **No secret at boot.** EC2 user-data carries only the JIT endpoint. The
  JIT config is served once, to the instance's own private IP.
- **No job logs kept.** firerunner keeps no job output; GitHub holds the job
  logs. A Firecracker VM's serial console is kept only if you set
  `--log-dir`.

In your workflows, grant `GITHUB_TOKEN` the least it needs (start from
`permissions: {}` and add per job). Prefer OIDC with narrowly scoped cloud
roles over long-lived secrets.

## Requirements

- Linux bare-metal host with KVM (`/dev/kvm`).
- `firecracker` binary, a guest kernel (vmlinux), and a golden rootfs image
  with `actions/runner` + a JIT-reading boot service pre-installed (see
  [`images/`](images/README.md) for the build tooling and rebuild policy).
- A reflink-capable filesystem (btrfs or XFS) for the work directory.
- `iproute2` (`ip`) and `nftables` (`nft`) on the host; `CAP_NET_ADMIN` and the
  ability to set `net.ipv4.ip_forward`.

## Build & test

```bash
make build         # build the binary
make test          # unit tests with -race
make cover-check   # unit coverage with a minimum threshold (COVER_MIN)
make e2e           # end-to-end tests (requires a real KVM host; -tags e2e)
```

Tests follow a functional-core / imperative-shell split: pure logic (the
Firecracker API sequence, the scale-up plan, config parsing) is unit-tested to
near-100%, host I/O is exercised through injected seams (a `commandRunner` fake
and an `httptest` server over a unix socket), and real microVM boots live in
`test/e2e` behind the `e2e` build tag. CI (`.github/workflows/ci.yml`) runs
build, vet and the coverage gate on every push and PR.

## Usage

```bash
firerunner \
  --url https://github.com/ORG/REPO \
  --name firerunner \
  --max-runners 4 --vcpu 4 --mem-mib 8192 \
  --kernel /var/lib/firerunner/vmlinux \
  --golden /var/lib/firerunner/golden.ext4 \
  --ext-iface enp2s0 --log-dir /var/log/firerunner \
  --egress api,actions,git,dns,packages,ntp --meta-refresh 24h \
  --app-client-id ... --app-installation-id ... --app-private-key /path/key.pem
```

All flags can also be set via `FR_*` environment variables (see
`config.example.env`).

Subcommands: `firerunner status` / `doctor` inspect a deployment (see below),
`firerunner cache-server` runs the [self-hosted dependency
cache](#self-hosted-dependency-cache---cache-port-opt-in), and `firerunner
version` / `help` do the obvious.

### Onboarding: creating and installing the GitHub App

firerunner registers runners through the Actions runner scale-set API. A GitHub
App is the preferred credential (short-lived installation tokens, no personal
account tied to the runners). Set one up once per org (or repo):

1. **Create the App** — *Settings → Developer settings → GitHub Apps → New GitHub
   App*. A homepage URL and a name are all that's required; you can disable the
   webhook. Grant the least-privilege permission for the scope you register at:

   | `FR_URL` scope | Required permission |
   | --- | --- |
   | `https://github.com/ORG` (org-wide) | **Organization → Self-hosted runners: Read & write** |
   | `https://github.com/ORG/REPO` (single repo) | **Repository → Administration: Read & write** |

   That permission, plus *Metadata: Read* (which every App has), is all
   firerunner uses. It registers the scale set, generates each runner's JIT
   config, and removes idle runners. **Grant nothing else.**
   - Jobs pull images and packages with their own `GITHUB_TOKEN`, not with
     the App.
   - An extra permission only widens what a stolen key can do. For example,
     *Packages: Read* exposes every private package and image.
   - `firerunner doctor` warns about any extra permission and names what it
     allows.
   - A future feature that needs more access will be opt-in, behind a
     separate App or an explicit permission request. It will never widen
     this one.

2. **Generate a private key** — on the App page, *Generate a private key*. This
   downloads a `.pem`. Install it where **only** the `firerunner` user can read
   it — `install` sets the mode directly instead of inheriting your umask like a
   plain `cp` would:

   ```bash
   sudo install -d -m0750 -o root -g firerunner /etc/firerunner
   sudo install -m0600 -o firerunner -g firerunner \
     ~/Downloads/your-app.*.private-key.pem /etc/firerunner/app-key.pem
   ```

3. **Install the App** — *Install App* → choose the org/account and grant it
   access to the repositories that will use the runners (or *All repositories*
   for org-wide use).

4. **Collect the three identifiers** and map them to config:

   | Where to find it | Flag | Env |
   | --- | --- | --- |
   | App page → *Client ID* | `--app-client-id` | `FR_APP_CLIENT_ID` |
   | Installed App URL `.../installations/<id>` (or *Install App* → gear) | `--app-installation-id` | `FR_APP_INSTALLATION_ID` |
   | The downloaded `.pem` path | `--app-private-key` | `FR_APP_PRIVATE_KEY` |

5. **Create a runner group** (optional but recommended) under the org's
   *Actions → Runner groups*, restrict it to the intended repositories, and pass
   its name via `--runner-group` / `FR_RUNNER_GROUP` (defaults to `default`).

With the App configured, point `FR_URL` at the org or repo and start firerunner
(see [Running as a service](#running-as-a-service-systemd)). A PAT
(`FR_TOKEN=ghp_...`) works too but is discouraged for production.

### Runner tiers (developer-selectable via `runs-on`)

A single firerunner process can serve several **tiers** — each its own GitHub
scale set with its own microVM shape and golden image — so developers pick the
runner they need straight from the workflow:

```yaml
jobs:
  build:   { runs-on: firerunner }          # default tier
  heavy:   { runs-on: firerunner-8c16g }    # 8 vCPU / 16 GiB
  web:     { runs-on: firerunner-node }     # Node-baked golden image
  compat:  { runs-on: firerunner-ubuntu }   # faithful ubuntu-latest full image
```

Point `--tiers` (or `FR_TIERS`) at a JSON catalog:

```json
[
  { "name": "firerunner",       "vcpu": 2, "mem_mib": 4096,  "golden": "/var/lib/firerunner/golden.ext4",           "min": 1, "max": 8 },
  { "name": "firerunner-8c16g", "vcpu": 8, "mem_mib": 16384, "golden": "/var/lib/firerunner/golden.ext4",           "min": 0, "max": 2 },
  { "name": "firerunner-node",  "vcpu": 2, "mem_mib": 4096,  "golden": "/var/lib/firerunner/golden-node.ext4",      "min": 0, "max": 4 },
  { "name": "firerunner-ubuntu","vcpu": 4, "mem_mib": 8192,  "golden": "/var/lib/firerunner/ubuntu-rootfs-full.ext4","min": 0, "max": 2 },
  { "name": "firerunner-ubuntu-min","vcpu": 4, "mem_mib": 8192, "golden": "/var/lib/firerunner/ubuntu-rootfs-minimal.ext4","toolcache": "/var/lib/firerunner/toolcache.ext4","min": 0, "max": 2 }
]
```

See [`examples/tiers.json`](examples/tiers.json). All tiers share the host
kernel, network and — importantly — the `--max-runners` **slot budget**:
`vcpu`/`mem_mib`/`golden` vary per tier, but the total number of
concurrent microVMs across every tier is capped at `--max-runners` (the tiers'
warm pools, `min`, must fit within it, and no single tier's `max` may exceed it).
When `--tiers` is unset, firerunner
runs the single tier derived from `--name`/`--vcpu`/`--mem-mib`/`--golden`, so
existing deployments are unchanged.

> The golden image is operator-scoped by design: a repo picks a tier from the
> trusted catalog you publish, but never injects its own rootfs. Define new
> tiers by building an image (see [`images/`](images/)) and adding an entry.

Each tier is reachable with `runs-on: <name>`. An optional `"labels"` array
adds **extra** `runs-on` aliases for a tier (the tier name is always advertised
too), e.g. `"labels": ["node"]` lets a job target the tier with either
`runs-on: firerunner-node` or `runs-on: node`.

The `firerunner-ubuntu` tier above points at the **faithful `ubuntu-latest`
full image** — an Ubuntu 24.04 (glibc 2.39) rootfs with the hosted tool cache
and the runner-images installer subset baked in (built by
[`images/build-ubuntu-rootfs.sh --toolset full`](images/build-ubuntu-rootfs.sh)).
Use it for jobs that need the closest parity with GitHub-hosted `ubuntu-latest`;
the lean Debian `firerunner` tier stays the default for everything else.

For Ubuntu glibc parity **without** the full image's bulk, the
`firerunner-ubuntu-min` tier above points at a
[`--toolset minimal`](images/build-ubuntu-rootfs.sh) golden: a thin Ubuntu 24.04
base (runner + git + build-essential + system python3 + Node.js runtime, **no
Docker or baked language toolchains**, ~1.6 GiB on disk) meant to be paired with
a [`--toolcache` drive](#pre-seeded-tool-cache---toolcache-opt-in) so `setup-*`
actions resolve languages on demand. Give it a drive with the tier's own
`"toolcache"` field (as above) — a per-tier binding that overrides the
process-wide `--toolcache`/`FR_TOOLCACHE` default, so a lean tier can attach a
drive while a full tier keeps its baked-in cache. It is ideal for the common
build/test/lint/SAST jobs that never touch Docker; Docker jobs stay on
`firerunner-8c16g-docker` or the full `firerunner-ubuntu` tier. Measured on a
real CodeQL run it matches the full image's speed at a fraction of the size
(CodeQL bundle served from the drive, no per-run download).

#### Several hosts behind one `runs-on` (active-active)

GitHub lets two scale sets share a name as long as they sit in **different
runner groups**; jobs for that name are then assigned to whichever one is free
and either host keeps serving if the other goes away. Run the same tier catalog
on each host with a different `--runner-group` / `FR_RUNNER_GROUP` and no
workflow has to change.

What makes this work under load is that each tier advertises its **live**
capacity to GitHub on every poll — the microVMs it already has in flight plus
whatever the host's `--max-runners` pool could still launch, capped at the
tier's `max` — rather than the static `max` alone. A host whose pool is
exhausted therefore reports "no room", GitHub assigns nothing more to it, and
the sibling host picks the job up instead of it queuing behind a full pool.
`firerunner` logs `advertising capacity` whenever the number changes. The
advertised value never drops below 1: the back-end's reading of a zero capacity
is undocumented, so a tier with nothing running on a full host may still be
handed one job, which then waits for the next free slot. And because every tier
on a host counts the same free slots, a burst that hits two tiers within one
poll can still be over-assigned by up to the free count until their next poll —
the pool is not split between tiers, since a split would strand jobs on a single
host whenever the busy tier had exhausted its share while other tiers sat idle.

### EC2 tiers (`"provisioner": "ec2"`)

A tier can launch **one ephemeral EC2 instance per job** instead of a microVM.
It sits in the same catalog as Firecracker tiers, and developers still pick it
with `runs-on: <name>`. firerunner runs on a small **control VM inside the
runner VPC**, and the instances reach it over the VPC. A catalog of only EC2
tiers needs no KVM, `--kernel` or `--ext-iface`.
See [`examples/tiers-ec2.json`](examples/tiers-ec2.json):

```json
{
  "name": "firerunner-cloud-4c",
  "provisioner": "ec2",
  "min": 0, "max": 40,
  "ec2": {
    "instance_types": ["c8a.xlarge", "c7a.xlarge", "m8a.xlarge"],
    "ami": "/firerunner/ami/runner",
    "subnets": ["subnet-0aaa…", "subnet-0bbb…"],
    "security_groups": ["sg-0ccc…"],
    "market": "spot-then-on-demand",
    "root_volume_gib": 40
  }
}
```

| field | meaning |
|---|---|
| `instance_types` | tried in order; the first with capacity wins |
| `ami` | an image ID (`ami-…`) or an SSM parameter holding one, re-read every 5 minutes so a rebuilt image needs no config change |
| `subnets` | tried in order for each type; one per AZ spreads launches across zones |
| `security_groups` | for the instances; they need **no inbound rules** |
| `market` | `spot`, `on-demand`, or `spot-then-on-demand` (every spot option before any on-demand one) |
| `root_volume_gib`, `root_volume_type` | optional root EBS override (default: the AMI's size, type `gp3`) |
| `pool` | how many pre-warmed, stopped instances to keep ready (default 0, every job cold-launches); see below |

`vcpu`, `mem_mib`, `golden` and `toolcache` are Firecracker fields and are
rejected on an EC2 tier. EC2 tiers do not count against `--max-runners`, which
stays the Firecracker hosts' slot budget; each EC2 tier is bounded by its own
`max`.

**How an instance gets its job.** The JIT runner config **never goes into
user-data**:

1. firerunner launches the instance with IMDSv2 required (hop limit 1, so
   job containers cannot reach IMDS), **no instance profile**, and user-data
   holding one line, `FIRERUNNER_JIT_URL=http://<--jit-host or --jit-addr>/jit`.
2. It registers the config under the instance's private IP. The instance's
   boot service ([`images/assets/firerunner-ec2-run.sh`](images/assets/firerunner-ec2-run.sh))
   fetches it. The config is served **once**, to that IP only: another
   address gets 403, a second fetch gets 404, and a config nobody fetches
   within `--jit-ttl` (default 5m) expires. The VPC delivers real source
   addresses, so a guest cannot claim another instance's config.
3. The runner takes one job and the instance powers off. Instances are
   launched to terminate on shutdown, for both on-demand and one-time spot. If
   one ever stops instead, firerunner terminates it.

firerunner also terminates an instance in these cases:

- it is idle when the scale set shrinks;
- it never fetched its config (this backs off, as a failed microVM boot does);
- it is older than `--max-vm-lifetime`.

Instances and volumes are tagged `firerunner:tier`, `firerunner:runner` and
`firerunner:owner` (this host's name).

**Stopped pool (`"pool": N`).** A cold instance takes about 40 s from launch
to running its job. A pool keeps N instances booted once and stopped, so a job
starts one in seconds:

- **Warm.** A pool instance boots with no user-data. Its boot script reads the
  runner's files, so EBS has fetched their blocks from the snapshot, then powers
  off. Spot pools use persistent requests that stop on interruption, and are
  still billed at the spot price. A stopped instance costs only its EBS volume.
- **Claim.** A job takes a ready instance. firerunner tags it, writes the JIT
  endpoint into its user-data, and starts it. Then it cancels the spot request,
  or AWS would replace the instance after terminate. The order matters: AWS
  will not start a stopped spot instance whose request is already cancelled.
  When the job ends the instance stops, and firerunner terminates it.
- **Refill and restart.** The pool refills in the background. Its state lives
  in the `firerunner:pool` tag, so a restarted firerunner finds its ready
  instances again and terminates any a crash left mid-claim.
- **Fallback.** With the pool empty, or if a stopped instance cannot start
  (spot capacity), the job cold-launches instead.

**Sizing by schedule (`"schedule"`).** A tier can keep more hot runners
(`min`) and a bigger stopped pool at busy times. It works on any tier;
`pool` needs an EC2 tier.

```json
{"name": "cloud", "provisioner": "ec2", "min": 0, "max": 20,
 "ec2": {"pool": 2, "...": "..."},
 "schedule": {"timezone": "Asia/Taipei", "windows": [
   {"days": "mon-fri", "hours": "06:00-13:00", "min": 2, "pool": 12},
   {"days": "mon-fri", "hours": "20:00-23:00", "min": 2, "pool": 12}]}}
```

- **Windows.** The first window covering the current time sets `min` and
  `pool`; one it leaves out keeps the tier's own value. Outside every window,
  the tier keeps its own `min` and `ec2.pool`.
- **Days** are `daily` or a list such as `mon-fri` or `sat,sun`.
- **Hours** are `HH:MM-HH:MM`, with the end exclusive. A window that ends at
  or before its start runs past midnight and belongs to the day it starts on.
- **Applying changes.** firerunner checks every 30 s and logs each change.
  `status` shows the current window.
- **Shrinking** terminates stopped pool instances first, then idle hot
  runners. A runner with a job is never stopped.
- **Validation.** `firerunner config check` rejects:
  - a window `min` above the tier's `max`;
  - a `pool` on a Firecracker tier;
  - Firecracker tiers whose largest window `min`s sum above `--max-runners`.

**Cleaning up after a crash (`--ec2-reaper`).** A reaper sweeps this host's
`firerunner:owner` instances at startup and every 5 minutes. It skips what the
running process tracks, and anything younger than 2 minutes.

- **A job instance left stopped:** firerunner cancels its spot request and
  terminates it.
- **A job instance still running:** firerunner asks GitHub to deregister its
  runner. If GitHub agrees, the runner had no job and the instance is
  terminated. If the runner is busy, its job is left to finish; it powers off,
  and the next sweep ends it. Past `--max-vm-lifetime`, it is terminated
  regardless.
- **A pool instance of a tier that no longer keeps a pool:** terminated.
- **A persistent spot request whose instance is gone:** cancelled.

Everything is found by the `firerunner:owner` tag, which defaults to the host
name. **Set `--owner` (`FR_OWNER`) to a stable name when the control VM can be
replaced**, so a new host takes over its predecessor's pool and leftovers.
Use `--ec2-reaper dry-run` to only log what it would do. With `off`, nothing
cleans up after a crash: not even pool instances left mid-claim.
No instance is terminated while its spot request may still be open.

When a type has no capacity in a subnet, firerunner tries the next subnet, then
the next type: `InsufficientInstanceCapacity`, `Unsupported` and
`InsufficientFreeAddressesInSubnet` all move on. Any other error (a quota,
permissions) fails the launch, and the scheduler backs off.

**Flags:**

- `--jit-addr` / `FR_JIT_ADDR` (required with an EC2 tier): the control VM's
  VPC `IP:port`, for example `10.0.0.5:8098`.
- `--jit-host` / `FR_JIT_HOST`: a private DNS name instances use instead of
  that IP, for example `control.firerunner.internal`. It keeps one address
  across control VM replacements; it must resolve to `--jit-addr`'s IP.
- `--jit-ttl` / `FR_JIT_TTL`: how long a config waits to be fetched.
- `--aws-region` / `FR_AWS_REGION`: defaults to the SDK's sources, then the
  control VM's own region.

**Running the control VM.** [`deploy/cloudformation/`](deploy/cloudformation/README.md) has a
CloudFormation template that does this for you:

- an Auto Scaling group of one, which replaces a failed VM;
- a private DNS name for `--jit-host`;
- `deploy-control.sh`, which deploys a release in one command.

**Control VM permissions.** Credentials come from the default chain,
normally the control VM's instance role. It needs no `iam:PassRole`, because
the runners get no role:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["ec2:RunInstances", "ec2:CreateTags"],
      "Resource": "*" },
    { "Effect": "Allow",
      "Action": ["ec2:DescribeInstances", "ec2:DescribeImages", "ec2:DescribeSpotInstanceRequests"],
      "Resource": "*" },
    { "Effect": "Allow",
      "Action": ["ec2:TerminateInstances", "ec2:StartInstances", "ec2:ModifyInstanceAttribute"],
      "Resource": "*",
      "Condition": { "StringEquals": { "aws:ResourceTag/firerunner:owner": "<control-vm-hostname>" } } },
    { "Effect": "Allow",
      "Action": "ec2:CancelSpotInstanceRequests",
      "Resource": "*" },
    { "Effect": "Allow",
      "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:*:*:parameter/firerunner/*" }
  ]
}
```

Scope `RunInstances` to your subnets, security groups and AMIs as tightly as
your account allows.

**Security groups.** The runners' group needs no inbound rules. The control
VM's group must allow TCP to the `--jit-addr` port **from the runners' group
only**. Never expose that port publicly.

**Verifying a deployment end to end.** These steps create real, billable
resources. [`scripts/e2e/`](scripts/e2e/README.md) has tested helpers for them:
a control VM of your own (a stack made from `deploy/cloudformation/control.yaml`), remote
commands on it, instance consoles, run watching and timing with deadlines, and
an inventory and cleanup of your own owner's instances. When several people or
agents share one test account, follow its rules: each has its own `FR_OWNER`
and tier names, and cleans up only its own owner's resources.

1. Build the runner AMI with `firerunner-ec2-runner.service` enabled, and
   publish its ID to the SSM parameter.
2. On the control VM, run `firerunner` with an EC2-only catalog (`max: 1`)
   against a test repository.
3. Push a workflow with `runs-on: <tier>`. Expect, in the firerunner log:
   `instance launched` → `served jit config` → `instance gone`. The job should
   be green.
4. Check that no secret is in user-data:
   `aws ec2 describe-instance-attribute --instance-id <id> --attribute userData`
   decodes to the single `FIRERUNNER_JIT_URL=` line.
5. Check that nothing is left behind: the instance is `terminated`, and the
   runner no longer appears under the repository's runners.

### Inspecting a deployment (`status`, `doctor`)

Two read-only subcommands help operators inspect and diagnose a runner host.
Both read the same flags / `FR_*` environment as the daemon, so run them with
the same environment as the service (e.g. via the systemd `EnvironmentFile`):

```bash
firerunner status   # config, kernel/golden images, network wiring, live microVMs
firerunner doctor   # preflight health checks; exits non-zero if any FAIL
```

Pass `--json` to either subcommand for machine-readable output (dashboards,
monitoring, or scripting a deploy gate):

```bash
firerunner status --json | jq .microvms.active
firerunner doctor --json | jq -r '.checks[] | select(.level=="FAIL") | .name'
```

`status` prints the resolved scale-set config, the golden/kernel/tool-cache
images (path, size, mtime), the network wiring, and the microVMs currently on
the host (cross-checked against firecracker processes and tap devices).

`doctor` runs a checklist — `/dev/kvm`, the firecracker/jailer binaries, the
kernel and golden images, the egress interface, `ip_forward`, `nftables`, a
writable and reflink-capable work dir, GitHub auth (presence plus a live
read-only credential check), and API reachability (against the derived API base,
so GitHub Enterprise Server works too) — and prints one `PASS`/`WARN`/`FAIL`
line each. It exits non-zero when any check fails, so it can gate a deploy.

**Reconciling with GitHub.** Every message from GitHub carries the scale set's
statistics: available, acquired, assigned, running, registered, busy and idle
counts. A reconciler per tier compares them with firerunner's own runners.
`status` shows both side by side, and `doctor` warns while they disagree.

- **Jobs available but never acquired for 60 s:** the message session is
  recycled, at most every 5 minutes, so a fresh one reports, and can re-offer,
  them.
- **A runner idle for 2 minutes that GitHub no longer knows:** it is cancelled,
  which frees its slot and its instance.
- **GitHub lists more runners than firerunner tracks**, confirmed by a later
  message: this is reported only. The instances behind them carry
  `firerunner:owner` tags.

GitHub keeps a job's assignment to the scale set until a runner takes it, not
in the message. So a firerunner that dies mid-message loses nothing: on
restart, the new session's statistics (`totalAssignedJobs`) bring the job back.
This was verified on EC2: firerunner was killed right after deleting the job's
message, the job waited queued, and it ran after the restart. In that setup
GitHub never sent `JobAvailable` messages (0 in 33 jobs); it assigns jobs to a
JIT scale set directly.

`FR_CHAOS_EXIT_AFTER_DELETE=1` makes the daemon exit right after deleting a
message, before acting on it. It is a fault-injection switch for recovery tests
only; never set it in production.

**Checking a configuration offline.** `firerunner config check` validates the
flags and `FR_*` environment exactly as startup does, without contacting GitHub
or AWS. Add `--catalog FILE` to validate one tier catalog on its own. That needs
no credentials and skips image-path checks, so an edit can be checked anywhere.
Pass `--json` for machine-readable output, which never includes credentials. Exit
status is 0 for valid, 1 for invalid and 2 for a usage error:

```bash
firerunner config check --catalog examples/tiers-ec2.json --max-runners 8
firerunner config check --json | jq -r 'if .ok then "ok" else .error end'
```

**Listener health.** Each tier's listener reconnects on its own. When its
message session fails, for example when GitHub is unreachable for longer than
the client library retries, it reopens the session with backoff (2 s doubling
to 2 min, with jitter). The tier's scheduler and running jobs are untouched, and
the new session's statistics restore the desired count. A tier stops only on an
error that retrying cannot fix. If GitHub rejects the credentials (401/403, not
rate limiting), only that tier stops: the others keep serving, and the process
exits once every tier has stopped. If the scale set itself is gone (404), the
daemon exits non-zero so its supervisor restarts it and startup registers the
set again. That happens when an instance that created the set deleted it on
shutdown while this one reused it, as in an overlapping redeploy.

The daemon writes each tier's state to `<--workdir>/listener-health.json` every
10 s. The state is `connected`, `reconnecting` or `failed`, and the file also
records reconnect counts, the time of the last successful poll and the last
error. `status` shows it, and marks it stale if the daemon stopped updating it.
`doctor` fails a `failed` tier and warns on a `reconnecting` one:

```bash
firerunner status --json | jq '.listeners.tiers[] | {name, state, reconnects, last_message}'
```

### Warm pool (`--min-runners`)

Every job runs in a fresh microVM, so by default a runner is cold-booted only
once a job is queued. The microVM itself boots in well under a second, but the
GitHub runner agent still has to start and open its session to GitHub before it
can accept work — so the *first* job after an idle period waits for that
one-time connect.

Set `--min-runners N` (env `FR_MIN_RUNNERS`) to keep `N` microVMs pre-booted and
already registered (`Listening for Jobs`). A queued job is then handed to a warm
runner immediately, and firerunner launches a replacement to refill the pool.
This trades idle capacity for lower pickup latency:

| `--min-runners` | Pickup                                   | Idle cost                              |
| --------------- | ---------------------------------------- | -------------------------------------- |
| `0` (default)   | cold boot + runner connect per job       | none                                   |
| `1`+            | job handed to a waiting runner           | `N ×` (`--vcpu` / `--mem-mib`) held idle |

Warm runners are still single-use and ephemeral: a pre-booted VM that has not
run a job is discarded like any other once it does. Size the pool to your peak
concurrency — `--max-runners` still caps the total.

**Stuck-VM backstops.** A healthy microVM self-destructs the instant its one job
ends, so a VM that is still alive when it should not be is holding a slot
hostage. Two host-side deadlines reclaim it:

- `--vm-connect-timeout` (env `FR_VM_CONNECT_TIMEOUT`, default `5m`) kills a
  booted VM whose runner has not registered with GitHub — the console never
  shows `Listening for Jobs` (or `Running job:`). This is the fast path for a
  VM wedged by lost DNS or egress, a rejected JIT config, or a broken golden:
  without it every slot fills with idle VMs that look healthy to the scheduler,
  and stays that way until the lifetime backstop. The kill is reported as a
  boot failure, so the scheduler backs off before relaunching instead of
  re-wedging all slots at once, and recovers by itself once the cause clears.
- `--max-vm-lifetime` (env `FR_MAX_VM_LIFETIME`, default `6h`) kills any VM
  still running after this long, however it got there (hung guest, wedged job).
  Keep it longer than your longest real job.

Both are `0` to disable. When both are set, the connect timeout must be shorter
than the lifetime (otherwise it could never fire first).

### Pre-seeded tool cache (`--toolcache`, opt-in)

On GitHub-hosted `ubuntu-latest`, `setup-go` / `setup-node` / `setup-python`
etc. are fast because the image ships a **hosted tool cache**
(`/opt/hostedtoolcache`, `RUNNER_TOOL_CACHE`) pre-populated with common language
versions: the action just finds the version and adds it to `PATH` instead of
downloading it. A stock golden has no such cache, so every `setup-*` pays the
download tax (a Go toolchain is ~5–9 s).

`--toolcache <image>` (env `FR_TOOLCACHE`) closes that gap without changing any
workflow YAML. Point it at a read-only ext4 image that mirrors the hosted
tool-cache layout and is **labelled `hostedtoolcache`**:

```
/                      # ext4 labelled "hostedtoolcache"
└── go/
    └── 1.26.7/
        ├── x64/       # the extracted toolchain
        └── x64.complete
```

firerunner attaches it to every microVM as a **shared read-only drive** (one
host image backs all VMs at once), and the golden mounts it by label at
`/opt/hostedtoolcache` as an overlay lower layer under a per-VM tmpfs upper,
exporting `RUNNER_TOOL_CACHE`. `setup-*` reads pre-seeded versions from the
drive (cache hit); when it needs a version the drive lacks, it downloads and
installs into the writable upper instead of failing on the read-only drive
(cache miss). The upper is tmpfs, so it is discarded with the VM:

```bash
firerunner ... --toolcache /var/lib/firerunner/toolcache-go.ext4
```

`--toolcache`/`FR_TOOLCACHE` is the **process-wide default**, applied to every
tier. A tier can override it with its own `"toolcache"` field in the
[tier catalog](#runner-tiers-developer-selectable-via-runs-on) — so a lean tier
attaches a drive while a full tier keeps its baked-in cache (empty falls back to
the global default, or no cache when neither is set).

Design properties:

- **Zero workflow change.** Only `runs-on:` differs from `ubuntu-latest`; the
  `setup-go@… with: go-version-file: go.mod` step is untouched.
- **Pure accelerator, always safe.** When the image is absent, or a requested
  toolchain/version is not in it, `setup-*` falls back to downloading — jobs
  still pass, just slower. A cache miss copy-up lands on the per-VM rootfs
  (disk-backed and discarded with the VM), not in guest RAM. The same golden
  works with or without the drive.
- **Opt-in, no bloat.** Include only the toolchains a project actually uses
  (derive them from its `go.mod` / `setup-*` steps); no kitchen-sink image.
- Measured on the node tier: `setup-go` for `go 1.26` dropped from **~6 s
  (download) to ~0 s (cache hit)** with the drive attached.

Under `--jailer` the image is staged read-only into each jail (reflink keeps
this near-free on a reflink-capable filesystem); otherwise the host path is
opened directly.

**Building the drive.** [`images/build-toolcache.sh`](images/build-toolcache.sh)
cuts a `hostedtoolcache`-labelled ext4 with exactly the tools and versions you
name — no OS rebuild, decoupled from the golden, so you can re-cut the cache
(new versions, more tools) and share one drive across every tier:

```bash
# Node + Go need only curl/tar/mkfs.ext4 (no Docker); many versions per tool:
sudo images/build-toolcache.sh --out /var/lib/firerunner/toolcache.ext4 \
  --node 20.18.0,22.22.2 --go 1.27.0
# Python is fetched from actions/python-versions and relocated in an
# ubuntu:24.04 container (so its layout matches the guest) -> needs Docker:
sudo images/build-toolcache.sh --out toolcache.ext4 --python 3.12
# CodeQL bundle (accelerates github/codeql-action / SAST); `latest` tracks the
# version the newest codeql-action pins, or pass a CLI version to pin it:
sudo images/build-toolcache.sh --out toolcache.ext4 --codeql latest
```

Tailor it to a team: scan the repos you serve for their `setup-*` steps
(`go-version-file: go.mod`, `.nvmrc`, `.python-version`) and bake only those
versions.

### Self-hosted dependency cache (`--cache-port`, opt-in)

`actions/cache` (and everything built on it — `setup-node`'s `cache: pnpm`,
`setup-go`'s build cache, `actions/setup-python`'s pip cache, etc.) normally
uploads and downloads archives from **GitHub's hosted cache**. On a self-hosted
runner every restore still crosses the internet to GitHub, and each repo is
capped at a 10 GB quota. firerunner can instead keep those archives on the
host's local disk, so dependency restore runs at LAN speed with no quota.

**How GitHub's cache works (v2).** The `@actions/cache` toolkit talks a small
Twirp/JSON RPC to the service at `ACTIONS_RESULTS_URL`
(`CreateCacheEntry` → `FinalizeCacheEntryUpload` → `GetCacheEntryDownloadURL`),
then streams the `tar+zstd` archive to/from a signed blob URL the RPC returns
(never through the RPC itself). Entries are keyed by `key`+`version`, namespaced
per repository and ref, immutable, and restored within each allowed ref by
exact key, key prefix, then ordered `restore-keys`.

**Our server.** `firerunner cache-server` is a small, dependency-free
implementation of exactly that protocol, backed by a directory on disk:

```bash
firerunner cache-server --dir /var/lib/firerunner/cache
```

It implements the three Twirp methods plus the Azure block-blob upload
(single-shot and staged `comp=block`/`comp=blocklist`) and ranged downloads,
tags each blob URL with a per-entry token, and persists an index so caches
survive restarts. Host-owned job records authorize each connection by its guest
source address, repository and ref. One server supports all repositories served
by its daemon, with the namespace derived exclusively from the job record.

It binds `127.0.0.1:8099` by default. Because each microVM slot reaches the host
on its own per-slot gateway IP (`172.16.<slot>.1`), serving guests means binding
a broader address — in practice `--addr 0.0.0.0:8099`, which the host firewall
**must** then keep off any LAN/WAN interface (the guest→host input chain already
restricts guests to the cache port, but the host's own external NIC is your
responsibility). Never leave `0.0.0.0:8099` reachable from an untrusted network.

By default the store is capped at 50 GB; once a newly finalized entry pushes the
total over the cap, the least-recently-used entries are evicted until it fits
(mirroring GitHub's per-repo LRU). Tune it with `--max-size` (e.g. `--max-size
100GB`, or `--max-size 0` for unlimited):

```bash
firerunner cache-server --dir /var/lib/firerunner/cache --max-size 50GB
```

The server also exposes `GET /stats` (JSON: entry count, bytes, hits, misses,
saves, evictions) and `GET /metrics` (Prometheus text, no client library
needed) for scraping. `firerunner status` reads `/stats` and shows the live
entry count, size vs. cap, and hit rate under the `dep cache` row.

**Pointing microVMs at it.** Two pieces are needed because the *real* GitHub
runner overwrites `ACTIONS_RESULTS_URL` with the value from its per-job message:

1. **Config** — run firerunner with `--cache-port 8099` (env `FR_CACHE_PORT`).
   firerunner publishes the port to each microVM via MMDS; the guest boot script
   builds `ACTIONS_RESULTS_URL` from its own default gateway (the host's per-slot
   tap IP) and this port, so the URL always points back at the host. For a
   different address on the same host, use `--cache-url http://host:8099`
   (env `FR_CACHE_URL`). Ref authorization requires the original guest source
   address and the same host-owned job directory; a remote cache behind SNAT
   or a reverse proxy is **not supported** by this identity scheme.
2. **A cache-redirect golden** — build the rootfs with
   `build-ubuntu-rootfs.sh --cache-redirect`. This renames the
   `ACTIONS_RESULTS_URL` string inside the runner agent so GitHub's URL lands in
   a dead variable while `actions/cache` still reads the real one firerunner
   exported. Without this patch the runner overrides our URL and caching stays on
   GitHub.

Both pieces are required together. A cache-redirect golden run **without**
`--cache-port`/`--cache-url` disables caching entirely: the runner no longer
sets `ACTIONS_RESULTS_URL` (it writes the dead name) and firerunner exports
nothing, so `actions/cache` has no endpoint. Jobs still run, just with no cache.

```bash
# 1. build a cache-redirect golden (pair it with a --toolcache drive as usual):
sudo images/build-ubuntu-rootfs.sh --toolset minimal --cache-redirect \
  --out /var/lib/firerunner/ubuntu-rootfs-minimal.ext4
# 2. configure/start the daemon first, then the separate cache service described
#    below. Its guest-facing listener must be firewalled off LAN/WAN interfaces.
firerunner ... --golden /var/lib/firerunner/ubuntu-rootfs-minimal.ext4 --cache-port 8099
# In another shell, after the jobs directory exists (use the daemon UID):
sudo -u firerunner-cache firerunner cache-server --addr 0.0.0.0:8099 \
  --dir /var/lib/firerunner-cache --jobs-dir /run/firerunner/jobs \
  --jobs-owner-uid "$(id -u firerunner)"
```

`status` shows the resolved cache configuration and `doctor` probes the
cache-server for reachability; both note that the cache is off by default and
that jobs fall back to GitHub's hosted cache when it is not configured.

**Artifacts keep going to GitHub.** `ACTIONS_RESULTS_URL` is not only the cache
endpoint: `actions/upload-artifact` and `download-artifact` (v4+) talk to a
second Twirp service, `ArtifactService`, at the same URL. A cache-redirect
golden therefore sends artifact RPCs to the cache-server too — and artifacts are
user-facing deliverables that must remain visible in the GitHub UI, the API and
`gh run download`, so storing them locally would silently break every consumer
outside the job. The cache-server instead **forwards** that one service to the
real GitHub Results endpoint and relays the reply verbatim, bearer token and
Twirp error envelopes included. Only the small JSON RPCs take this hop; the
archive itself flows between the guest and the signed
`productionresultssa*.blob.core.windows.net` URL GitHub returns, exactly as it
does without any redirect (so an egress allowlist needs nothing new). The
upstream defaults to github.com's `results-receiver.actions.githubusercontent.com`.
This is a github.com feature for now: `upload-artifact@v4+` is not supported on
GitHub Enterprise Server, so there is no GHES ArtifactService endpoint to point
at. `--artifact-upstream` is still a flag rather than a constant so a
protocol-compatible upstream can be substituted (a test double, a future GHES
service) without a rebuild:

```bash
firerunner cache-server ... --artifact-upstream https://results.example.internal/
```

Setting `--artifact-upstream ""` refuses artifact RPCs outright (a Twirp
`unimplemented` error naming the flag) — useful only where jobs never upload
artifacts. `status` reports forwarded artifact RPCs and errors next to the cache
hit rate, and `/metrics` exposes them as `firerunner_artifact_rpcs_total` /
`firerunner_artifact_errors_total`.

**Jobs that run in a container.** A `container:` job does not run its steps in
the runner's process environment: the runner launches each one with
`docker exec -e KEY` for exactly the keys in its own step-environment table,
which (on a cache-redirect golden) holds only the renamed, dead
`ACTIONS_RESULTS_ORL`. The
`ACTIONS_RESULTS_URL` the boot script exported would never enter the container,
so cache would silently miss and `upload-artifact` would fail. Docker-toolset
goldens (`base`/`full`) therefore install a small shim ahead of `/usr/bin/docker`
on `PATH` that adds `-e ACTIONS_RESULTS_URL=…` and `-e ACTIONS_CACHE_SERVICE_V2=…`
(forwarding the runner's value, else `true`, so `actions/cache` speaks the v2
protocol the cache-server implements) to the runner's own `docker create` — the
job container's create-time environment is inherited by every later `docker
exec`. The shim acts only when the variable is set, only for
`create`, and only when the caller is `Runner.Worker`; a job's own `docker`
commands pass through untouched, and on a non-redirect golden it is inert.

**Security model.** The daemon receives `JobStarted` through GitHub's scale-set
control plane and resolves the run and repository through the authenticated
REST API. The guest's `ACTIONS_RUNTIME_TOKEN`, metadata, `Authorization`, and
`X-Forwarded-For` headers do not grant local cache authority. One cache-server
serves multiple repositories, using only the repository from a resolved record.
There is no global repository/default-ref override or anonymous fallback.

- A branch job writes only to its branch ref. A PR (including a fork PR) writes
  only to `refs/pull/<number>/merge`. Restore exhausts that ref's exact/prefix
  matches before its PR base and then the default branch. An own-ref prefix
  beats a default-ref exact hit. Sibling PRs and branches cannot read each
  other's entries, including with a blank restore key.
- Reservations belong to the creating job and VM generation. Uploads, finalize,
  GET and HEAD all check authority; a signed URL alone is insufficient. VM exit
  revokes the record before the slot is reused, late API responses cannot grant
  a new occupant the old rights, and a failed revocation quarantines the slot.
  TCP connections also bind to their initial VM generation: an old keep-alive
  or delayed request body cannot acquire the next job's scope. If embedding the
  handler, install `Server.ConnectionContext` as `http.Server.ConnContext`
  (the CLI does this automatically).
- The daemon renews records every 30 seconds with a 90-second lease. Atomic
  replacement prevents partial reads; the cache server rereads each request.
  Startup clears stale records under an exclusive directory lock. A daemon
  crash can leave the old VM authorized until lease expiry; systemd's cgroup
  cleanup is still required to reap orphaned VMs. Expired records never write.
- Missing, pending, invalid or expired identities cannot read or write. The
  authenticated repository/run/job is published at `JobStarted` before REST
  resolution, but grants no rights until resolution succeeds. Default-only
  reads require a resolved identity for that repository. A failed lookup is a
  cache miss/denied save, never a job failure. Resolution failures and timeouts
  are logged by the daemon; after three attempts/30 seconds the record is marked
  `unresolved` and stays denied for that job. Guest HTTP never triggers
  privileged REST retries.
- `JobStarted` reaches the daemon well after the guest has dequeued the job
  (tens of seconds is common), and a workflow's first `actions/cache` restore
  usually runs before that. A request from a **pending** lease therefore waits,
  polling the record, until it resolves, becomes `unresolved`, changes
  generation, or `--identity-wait` (default 60s, `0` denies at once) runs out;
  only then is it denied. The toolkit's HTTP client allows 3 minutes per request.
  Waiting grants nothing a resolved record would not: the same validation and
  connection-generation check apply afterwards.
- The first denial per address is logged with its reason (bounded to 4096
  outstanding addresses); a successful lookup resets it. `/stats` exposes
  `identity_denied` (total), `identity_denied_by_reason`, `identity_waits` and
  `identity_wait_seconds`; `/metrics` exposes
  `firerunner_cache_identity_denied_total{reason}`,
  `firerunner_cache_identity_waits_total` and
  `firerunner_cache_identity_wait_seconds_total`. Reasons: `missing` (no
  record), `pending` (the wait ran out or the client left), `unresolved`,
  `expired`, `invalid`, and `generation` (a connection outlived its VM). A
  rising `pending` count means `--identity-wait` is shorter than the
  `JobStarted` lag; `unresolved` points at App permissions or the REST API.
- REST `pull_requests` can be empty: the resolver checks commit-associated PRs
  and requires a unique match of head/base repository and head branch. A later
  push to a branch or PR does not invalidate earlier queued runs or reruns.
  PR base reads require the run association and current PR to agree on the base.
  Retargeted PRs, missing run base metadata and commit-association fallback get
  only PR-own and default reads; the current PR cannot grant a new base scope.
  API routing supports GitHub.com, Enterprise Cloud (`*.ghe.com`) and GHES,
  including the scaleset client's `GITHUB_ACTIONS_FORCE_GHES` override.
  Branch writes require an existing branch, no same-named tag, and proof that
  the run's commit belongs to that branch: run `head_branch` is a bare name, so
  a tag deleted after its run would otherwise pass as the branch. A `push` run
  needs a repository-activity record (push, force push, PR or merge-queue merge,
  branch creation) updating `refs/heads/<branch>` to the run's SHA within the
  last month, searched up to 1,000 records. Dispatch and schedule runs, and GHES
  without the activity endpoint, need the commit in the branch's current
  history (`compare`, skipped when the branch still points at it). An unproven
  run keeps default-only reads and gets no write. Tag runs,
  ambiguous PR associations and API errors get no write access. `pull_request_target`,
  `workflow_run`, `issue_comment` and other unrecognized events get resolved
  default-only reads. `JobWorkflowRef` is deliberately not a trigger ref: even
  same-repository reusable workflows can point to another branch. This does not
  implement GitHub `cache-mode` overrides from guest environment variables.
- Pass the same directory to the daemon (`--cache-jobs-dir`, `FR_CACHE_JOBS_DIR`,
  default `/run/firerunner/jobs`) and cache-server (`--jobs-dir`). Use daemon-owned
  **0750 directories and 0640 records**, with a dedicated read-only group. The
  writer preserves the directory's group on atomic replacement. No group/other
  writes or other-user access is accepted; 0700/0600 also works for same-UID
  tests. Run the cache server as its own user and set `--jobs-owner-uid` to the
  daemon UID (default: current UID). Wrong ownership or unreadable directories
  fail startup; records are checked again on each request. Never mount the
  directory into guests. Each daemon needs its own directory; all its tiers
  share it. The jailer template uses `/run/firerunner-<instance>/jobs`.
- Cache traffic must reach the server directly with its guest source IP intact.
  The firewall binds each tap (or host-veth in netns mode) to its exact guest
  address **before** established-connection and cache-port accepts, in open and
  allowlist modes. Keep the listening port off LAN/WAN interfaces; local host
  processes and the host administrator remain trusted.
- Default/base caches are intentionally readable by fork PRs: **do not store
  secrets in them**. Code authorized to write a trusted branch can still poison
  that branch's cache. This is ref isolation, not content authentication.

**Upgrade/migration.** Entries lacking `ref` cannot be attributed and are never
restored, finalized, or accessed through old blob URLs. They are not relabeled
as default-branch entries. The loader retains their IDs and size accounting to
avoid ID reuse; normal eviction can reclaim them. Prefer a new empty `--dir`
on upgrade, and remove the old directory separately once no old server uses it.
Stop the old cache server first to close its unauthenticated access, upgrade and
restart the daemon/firewall, then verify records appear for active VMs before
starting the new cache server on a fresh `--dir`. Plan for the daemon's graceful
drain to take up to the longest job. Caches may miss during this interval. Remove
old `--repository` and `--default-ref` flags; they are no longer accepted.
The server has no legacy unauthenticated-write mode.

For separate service users, create the `firerunner-cache` system group and user,
then add a daemon unit drop-in with `Group=firerunner-cache` and
`RuntimeDirectoryMode=0750`, and `UMask=0027` (also for a jailer instance). Keep App keys and any
secret environment files **0600**, owned by the daemon or root. Set the cache
service's `/etc/firerunner/cache.env` to non-secret values:

```ini
FR_CACHE_JOBS_DIR=/run/firerunner/jobs
FR_CACHE_OWNER_UID=<numeric UID from id -u firerunner; 0 for the root jailer daemon>
```

Install [`deploy/firerunner-cache.service`](deploy/firerunner-cache.service),
which has its own user/storage, no capabilities, and read-only `/run`. Only the
daemon owns `RuntimeDirectory=firerunner`; do not repeat it in the cache unit,
or stopping either unit could remove live records. The cache unit starts after
`firerunner.service`; adjust `After=` for a jailer instance. Configure the guest
firewall before exposing its listener.

The cache-enabled daemon's GitHub App additionally needs repository **Actions:
read**, **Contents: read** (ref, activity and compare checks), and **Pull requests: read**
(PR association), plus its existing runner-registration permissions. Install
it on every repository served. The resolver uses a separate REST client and
short-lived installation tokens restricted to the requested repository and
Actions/Pull requests/Metadata/Contents read permissions. Contents read remains
necessary for branch/tag probes and for the activity and compare endpoints. Concurrent matrix jobs share one resolution;
successful run policies are cached for two minutes (bounded to 1024 runs), while
job IDs, generations and record leases remain per job. Credentials never enter
job records.

**Artifact forwarding.** The artifact forwarder is not a general proxy: it relays exactly the five
  `ArtifactService` methods the toolkit calls (anything else, including an
  encoded dot-segment aimed at another service, is refused before forwarding),
  only via `POST`, only to the single operator-configured
  `--artifact-upstream`, with request bodies capped at 1 MiB and each RPC
  bounded by a 2-minute end-to-end deadline. The guest's
  `ACTIONS_RUNTIME_TOKEN` is passed through to that host alone (it is GitHub's
  credential for GitHub's service), no `X-Forwarded-*` headers are added, and
  hop failures come back as generic Twirp errors — the upstream address and
  transport details stay in the server log — so a guest cannot steer the hop
  elsewhere or learn anything about the host.

Uploads are bounded per-entry (`--max-entry-size`, default 10GB) and in total
(`--max-size`), and finalized entries are immutable.

The cache is a pure accelerator — every job still passes with caching disabled,
so when in doubt, leave it off.

### Running as a service (systemd)

`deploy/firerunner.service` is a hardened unit template (dedicated `firerunner`
user, least-privilege capabilities). To install:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin firerunner
sudo usermod -aG kvm firerunner
sudo install -m0755 firerunner /usr/local/bin/firerunner
# /etc/firerunner holds secrets (the env file and the App private key). Keep the
# directory traversable by the firerunner user only, never world-readable.
sudo install -d -m0750 -o root -g firerunner /etc/firerunner
# The env file carries FR_TOKEN / App credentials, so install it 0640 (systemd
# reads it as root; the firerunner group may read it) instead of letting cp
# inherit your umask and leave it world-readable.
sudo install -m0640 -o root -g firerunner config.example.env /etc/firerunner/firerunner.env   # then edit
sudo install -m0644 deploy/firerunner.service /etc/systemd/system/
sudo systemctl enable --now firerunner
```

Stopping the service sends `SIGTERM`; firerunner stops taking new work and
drains in-flight microVMs before exiting (`TimeoutStopSec` bounds the wait).
Restarts are safe: registration is idempotent and a session left behind by an
unclean exit is retried until GitHub expires it.

### Hardening: the Firecracker jailer (`--jailer`, opt-in)

firerunner already runs the VMM **non-root** (a dedicated `firerunner` user with
only `cap_net_admin`) and Firecracker installs **seccomp** filters by default, so
the two biggest sandboxing wins are on out of the box. The
[jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
adds the rest — it `chroot`s each microVM into `<chroot-base>/firecracker/<id>/root`,
gives it a private PID namespace, creates jail-local `/dev/kvm` and `/dev/net/tun`
nodes, and drops the VMM to an unprivileged uid/gid.

It is **off by default** because it inverts the privilege model: the *launcher*
must run as **root** (to `chroot`, `mknod` and drop privileges), whereas the
default deployment keeps firerunner unprivileged. Enable it for multi-tenant,
untrusted-code or shared-host deployments:

```bash
firerunner ... \
  --jailer --jailer-bin /usr/local/bin/jailer \
  --chroot-base /srv/jailer \
  --jail-uid "$(id -u firerunner)" --jail-gid "$(id -g firerunner)"
```

Notes:

- The `jailer` binary must be the **same version** as `firecracker` (it ships in
  the same release tarball) and Firecracker must be the static musl build.
- By default no network namespace is used (`--netns` is not passed), so the
  per-slot host-namespace tap devices and the egress allowlist work unchanged.
- **Per-VM network namespace** (opt-in, jailer only): `--netns` puts each
  microVM's tap inside its own network namespace, giving the strongest network
  isolation — a guest cannot see the host's other taps, interfaces or routes,
  only a point-to-point veth uplink:

  ```bash
  firerunner ... --jailer ... --netns
  ```

  The jailer joins the namespace natively via `--netns /var/run/netns/<ns>`
  (creating/entering a namespace needs `CAP_SYS_ADMIN`, which only the root
  jailer has — hence the jailer requirement). Each slot gets a private namespace
  holding the guest tap (owned by the jail uid/gid so the dropped-privilege VMM
  can still attach it) plus a veth pair on a transit `/30` to the host; the host
  routes the guest `/32` back through the veth. Guest egress keeps its source IP
  to the host (single NAT), so the masquerade and the guest-to-guest drop rules
  apply unchanged. Namespaces are torn down on teardown and swept on startup.
- **Per-VM cgroup limits** (opt-in, jailer only): `--jailer-cgroup` takes a
  semicolon-separated list of cgroup v2 `<file>=<value>` settings the jailer
  applies to each microVM's own cgroup, bounding one VM's blast radius on the
  shared host (a noisy-neighbour guard for multi-tenant use):

  ```bash
  firerunner ... --jailer ... \
    --jailer-cgroup "memory.max=2147483648;cpu.max=200000;pids.max=512"
  ```

  Semicolons (not commas) separate entries because values such as
  `cpuset.cpus=0-3,5` legitimately contain commas. The jailer places the VMM in
  `/sys/fs/cgroup/firecracker/<id>` and enables the needed controllers; the
  (empty) cgroup dir is reclaimed on teardown and on startup. Set `memory.max`
  **above** the guest RAM (`--mem-mib`) plus VMM overhead — a cap below the
  guest's memory will let the kernel OOM-kill the VMM under load.
- Per-launch overhead is ~single-digit milliseconds (chroot + staging the VMM),
  negligible against the microVM boot.
- The systemd unit must run as `root` when the jailer is enabled: use
  `deploy/firerunner-jailer@.service` (a root, jailer-forced unit with a trimmed
  capability set and cgroup-writable sandbox). The default
  `deploy/firerunner.service` runs as the unprivileged `firerunner` user for the
  non-jailer mode.

## License

[MIT](./LICENSE), copyright Kaik, Inc. The `LICENSE` file lists the directories
that carry a different license.

## Contributing

Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org/): sign off
every commit with `git commit -s`. The `dco` workflow checks this on each pull
request.
