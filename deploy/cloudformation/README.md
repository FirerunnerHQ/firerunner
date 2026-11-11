# Runner infrastructure on AWS

The CloudFormation templates for running firerunner's EC2 tiers. They are
the single source for this infrastructure, whether you launch them yourself
or deploy them from other tooling:

| template | creates | needs |
|---|---|---|
| [`network.yaml`](network.yaml) | a VPC with public subnets in 2–3 zones, and the runners' security group (no inbound) | — (or bring your own VPC) |
| [`control.yaml`](control.yaml) | the control plane: an Auto Scaling group of one VM, its role, its security group, a private DNS zone | a VPC, subnets, the runners' group |
| [`reaper.yaml`](reaper.yaml) (optional) | a scheduled Lambda that ends the install's instances running past a limit | the install's `Owner` |
| [`byoc-role.yaml`](byoc-role.yaml) (bring your own cloud) | a role a control plane in another account assumes, with an external ID, to run runners here | that account's ID, an external ID, `Owner` |

They take parameters only. Their outputs feed the next template, or
Terraform or StackSets.

## Control plane

[`control.yaml`](control.yaml) runs firerunner's control plane for EC2 tiers
as one VM in an Auto Scaling group of one:

- **Replacement on failure.** The group replaces a VM that fails EC2's
  checks, one whose boot fails (bad release, missing parameter, rejected
  config), and one whose firerunner unit fails ten times in half an hour.
  The new VM takes over the pool and leftovers, because the owner tag
  (`Owner`) stays the same.
- **A stable private name.** At boot the VM points `control.<zone>` (a
  Route 53 private zone) at itself. Runner user-data carries that name
  (`--jit-host`), never an IP.
- **Deploy = one command.** [`deploy-control.sh`](deploy-control.sh) sets
  the version and refreshes the instance.

Nothing account-specific lives in this directory: IDs, names and
credentials are stack parameters and SSM parameters.

## How a VM boots

1. User-data reads `<prefix>/control/version` (a release tag, for example
   `v0.2.0`). It downloads that release's binary for its architecture and
   `firerunner-control-bootstrap.sh`, and checks both against
   `<prefix>/control/sha256sums`. That parameter holds the release's
   `SHA256SUMS`, which `deploy-control.sh` pinned at deploy time, so an
   asset replaced afterwards is refused. Before it pins them,
   `deploy-control.sh` checks each file's build provenance (see
   [Deploy, roll back](#deploy-roll-back)).
2. [`control-bootstrap.sh`](control-bootstrap.sh) installs the binary and
   writes `/etc/firerunner/` from SSM. Then `firerunner config check` must
   pass, and only then does the name move and the unit start.
3. Any failure marks the instance unhealthy. The group replaces it, and the
   log says why (see [Debugging](#debugging)).

## The pool key

The stack generates a Secrets Manager secret, `PoolKey`, that signs EC2 pool
instances (`firerunner:pool-mac`, `firerunner:pool-disk-mac`; see the
README's *Stopped pool*) and job instances' runner tags
(`firerunner:runner-mac`). Only the control role may read it. At boot the VM
copies it to the root-only file `/etc/firerunner/pool-key` and points
firerunner at it (`FR_EC2_POOL_KEY_FILE`), so a restarted or replaced VM
reuses the pool instances this install launched, trusts no others, and its
reaper deregisters only the runners this install launched.

- **Upgrading.** Pool instances launched by a release before the key, or
  before `firerunner:pool-disk-mac`, lack a MAC. The first VM that checks it
  terminates them once and refills the pool.
- **A stack made before `PoolKey`** has no secret: firerunner then signs with a
  random key per process and rebuilds the pool after every restart. Update
  the stack to keep it.
- **Rotating the key** (a new secret value, then a VM refresh) discards the
  pool once, the same way.

## Job events (optional)

Store a Postgres URL as the SecureString `<prefix>/control/job-events-dsn`, and
firerunner keeps each job's stages, machine and result there. The code is under
[`proprietary/`](../../proprietary/LICENSE). A database that is down costs that
record, never a job.

## Keeping the App key in KMS (optional)

Import the GitHub App's private key into an AWS KMS key with `KeySpec` `RSA_2048`
and `KeyUsage` `SIGN_VERIFY`, and pass its ARN as the stack's `AppKMSKeyArn`. The
VM's role may then only `kms:Sign` with it (RS256 over a digest), firerunner signs
the App's JWTs there, and `<prefix>/control/app-private-key` is not read: no copy
of the key reaches the VM. Each signature is in CloudTrail.

## Tenants (optional)

Store a JSON array as the String `<prefix>/control/tenants`, and the VM serves
those installations too, on the same GitHub App and tier catalog:

```json
[{"id": "acme", "url": "https://github.com/acme", "installation_id": 1234, "tiers": ["firerunner-ubuntu-24.04-2"]}]
```

Each runs under limits, so that signing up is no route to free compute: by
default at most 2 runners at once across its tiers, only tiers of at most 2 vCPU,
and 30 minutes per job. Raise them in its entry, where 0 lifts one:

```json
"limits": {"max_runners": 10, "max_vcpu": 8, "max_job_duration": "2h"}
```

A tier above its vCPU limit is not served to it, with a warning in the log and in
`firerunner config check`. A job past the runner limit waits, and `firerunner
status` says why; a job past its duration has its runner ended, and fails. A
changed list takes effect when the VM is replaced, as its other parameters do.

Each gets its own scale sets and the owner tag `<owner>/<id>`, which the VM's role
and the reaper Lambda also cover. A tenant that stops is tried again by itself,
and shows in `firerunner status` meanwhile; the others are not touched. A tenant's
tiers run with caches off and no hot runners or pool, and a tier's `max` applies per
tenant. Take a tenant out of the list only once its instances are gone: only the
reaper Lambda still covers them.
## Before the first deploy

You need:

- A VPC whose subnets have egress to GitHub, with DNS support and DNS
  hostnames on (for the private zone), and the runners' security group.
  `network.yaml` makes both:

  ```sh
  aws cloudformation deploy --stack-name firerunner-network \
    --template-file deploy/cloudformation/network.yaml
  aws cloudformation describe-stacks --stack-name firerunner-network \
    --query 'Stacks[0].Outputs'   # VpcId, SubnetIds, RunnerSecurityGroupId
  ```

  Its subnets' network ACL denies outbound SMTP (TCP 25, 465 and 587), so
  job code cannot send mail from them; everything else is as the default
  ACL. A VPC of your own needs the same, as a security group can only
  allow.
- A runner AMI published to an SSM parameter under the prefix
  (see [`images/`](../../images/)).
- A release of firerunner: a `v*` tag publishes one.

Put the config in SSM, under the prefix (default `/firerunner`):

| parameter | type | holds |
|---|---|---|
| `<prefix>/control/env` | String | non-secret `FR_*` lines: `FR_URL`, `FR_APP_CLIENT_ID`, `FR_APP_INSTALLATION_ID`, tuning flags |
| `<prefix>/control/tiers` | String | the tier catalog (JSON); `ami` may name `<prefix>/...` parameters |
| `<prefix>/control/app-private-key` | SecureString | the GitHub App key (or `token`: a PAT) |
| `<prefix>/control/version`, `sha256sums` | String | written by `deploy-control.sh` |

The bootstrap sets `FR_OWNER`, `FR_JIT_ADDR`, `FR_JIT_HOST`,
`FR_AWS_REGION`, `FR_TIERS` and `FR_WORKDIR` itself; leave them out of
`env`.

`env` is data, never shell: each line is blank, a `#` comment, or
`FR_NAME=value`. A bare value holds letters, digits and `_ . / : @ % + = , -`;
in single or double quotes it may hold anything printable but quotes, a
backslash, `$` and a backtick, which it takes literally. Any other line fails
the bootstrap, and the log names its line number.

```sh
aws ssm put-parameter --name /firerunner/control/env --type String --value "$(cat env)"
aws ssm put-parameter --name /firerunner/control/tiers --type String --value "$(cat tiers.json)"
aws ssm put-parameter --name /firerunner/control/app-private-key --type SecureString --value "$(cat app.pem)"
```

SecureStrings must use the default `aws/ssm` key; the role has no
`kms:Decrypt` for a key of your own. The role reads parameters only under
the prefix, so publish the runner AMI there, not as an `/aws/service/...`
name. Spot tiers in a new account need the EC2 Spot service-linked role;
create it once with
`aws iam create-service-linked-role --aws-service-name spot.amazonaws.com`.

Create the stack, then deploy a release (below). Pick an `Owner` that names this install and **never
change it**: the owner tag is how a replacement VM finds the pool.

```sh
aws cloudformation deploy --stack-name firerunner --template-file deploy/cloudformation/control.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides \
  Owner=acme-prod VpcId=vpc-... SubnetIds=subnet-a,subnet-b \
  RunnerSecurityGroupId=sg-...
```

The group's first VM fails its boot, and keeps being replaced, until the
first `deploy-control.sh` sets a version. Pass `HostedZoneId=Z...` with `ZoneName=<its name>` to use an existing
private zone. Use `ImageId=/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64`
with an x86 `InstanceType`.

**The dependency cache (`Cache`, default `off`).** With `disk` or `s3`, the
control VM also runs `firerunner cache-server` for EC2 tiers that set
`ec2.cache`:

- **The server.** It runs as its own `firerunner-cache` user on the VM's VPC
  address, port `CachePort` (8099), and only when a tier sets `ec2.cache`.
  A security group rule lets only the runners' group in. It reads the job
  identities the daemon writes in `/run/firerunner/jobs` and cannot read
  `/etc/firerunner`.
- **The disk.** The root volume is `RootVolumeGiB` (30). The cache gets a
  share of it: blobs up to 40% on `disk`, local copies up to 30% with `s3`,
  and one upload stages at most 15%.
- **`disk`.** Blobs live on the VM and go with it. cache-server cannot reach
  instance metadata, so it holds no AWS credentials.
- **`s3`.** The stack makes a private, encrypted bucket and a cache role
  that may read, write and delete under `firerunner-cache/` and list only
  that prefix (the README's least policy). The control role has no S3
  access; it may assume the cache role and no other, and the cache role
  trusts only it. The daemon assumes the cache role (`FR_CACHE_ROLE_ARN`)
  and keeps one-hour session credentials in
  `/run/firerunner/cache-s3-credentials.json`, readable by the cache user,
  rewriting them every 15 minutes. cache-server signs with them
  (`--s3-credentials-file`) and, as with `disk`, cannot reach instance
  metadata, so it never holds the control role's credentials. `firerunner
  status --json` shows the refresh under `listeners.cache_credentials`
  (file, last refresh, expiry, last error), and `doctor`'s
  `cache-credentials` check fails when the file is missing or expired or the
  latest refresh failed. A
  replacement VM serves what its predecessor saved. The bucket is **kept**
  when the stack is deleted; empty and delete it by hand (the `CacheBucket`
  output names it). A lifecycle rule aborts multipart uploads left
  incomplete after a day.
- **Upgrading an `s3` stack** made before the cache role: deploy a release
  that has `--s3-credentials-file` first, then update the stack and refresh
  the VM (`deploy-control.sh`). An older release on the new stack has no S3
  access; a newer release on an older stack keeps using the instance role
  and says so in the boot log. Between the stack update and the refresh,
  the running VM's cache-server still signs with the instance role, which
  has lost its S3 access: every restore misses and every save fails until
  the new VM is up, so refresh right after the update.
- **The address.** The bootstrap tells runners the cache-server address
  (`FR_EC2_CACHE_ADDR=<jit name>:<port>`) only when a tier sets
  `ec2.cache`. Such a tier with `Cache=off` fails `config check`.
- **Changing `Cache`** updates the launch template, not the running VM:
  refresh it (`deploy-control.sh`) for the change to take effect. Switching
  from `s3` away and back makes a new bucket and keeps the old one.

## Deploy, roll back

```sh
deploy/cloudformation/deploy-control.sh firerunner v0.2.1   # waits until v0.2.1 has run 60 s, no restarts
deploy/cloudformation/deploy-control.sh firerunner v0.2.0   # roll back: the same, older
```

The refresh terminates the old VM before launching the new one: two
listeners on one scale set fight over its session. Expect about two
minutes with no listener. Jobs already running keep running on their own
instances; the new VM's reaper ends what they leave behind. Queued jobs
wait and are picked up by the new listener.

Changing `env` or `tiers` takes a refresh too. Run `deploy-control.sh` with
the current version.

`SHA256SUMS` comes from the same release as the files it lists, so
`deploy-control.sh` does not take it on its own word. On your machine, before
it changes anything, it downloads both binaries and
`firerunner-control-bootstrap.sh`, checks each against `SHA256SUMS`, and runs
`gh attestation verify` on each: the attestation must come from the release
repo's `.github/workflows/release.yml`, for the tag being deployed, on a
GitHub-hosted runner. Any failure stops the deploy. This needs
[`gh`](https://cli.github.com/), logged in. Releases before v0.1.9 carry no
attestations: to deploy or roll back to one, set `DEPLOY_SKIP_ATTESTATION=1`,
which pins `SHA256SUMS` unchecked, as before.

## Debugging

Use Session Manager, with no SSH and no open ports:
`aws ssm start-session --target <instance>`.

- Why a boot failed: `/var/log/cloud-init-output.log`, or
  `aws ec2 get-console-output --instance-id <id>`. Lines are prefixed
  `firerunner user-data:` and `control-bootstrap:`. A replaced VM's console
  output stays readable for about an hour after it terminates.
- Why replacements keep happening: the group's activity history
  (`aws autoscaling describe-scaling-activities --auto-scaling-group-name <stack>-control`).
- The running plane: `journalctl -u firerunner`, and
  `FR_WORKDIR=/var/lib/firerunner firerunner status`.

A config the VM rejects makes the group replace it again and again, every
few minutes, until the parameter is fixed. Fix it, then deploy.

Turn on immutable releases for the release repository. The pinned checksums
already refuse a replaced asset, but immutability also protects the first
deploy of a version.

## Deleting the stack

```sh
aws cloudformation delete-stack --stack-name firerunner
aws cloudformation wait stack-delete-complete --stack-name firerunner
```

- **The JIT name goes with it.** The VM writes `control.<zone>` itself, so
  CloudFormation does not own that record. A small custom resource
  (`JitNameCleanup`, an inline Lambda) deletes it once the group and its VM
  are gone, and before the zone. Its role may only list the zone and delete
  that one A record. A record that is already gone, or was never written,
  is fine. In a zone you passed as `HostedZoneId`, no other record is
  touched.
- **Stacks made before `JitNameCleanup`** fail to delete their own zone
  (`HostedZoneNotEmpty`) while the record is there. Update the stack to the
  current template first, or delete the `A` record by hand and delete the
  stack again.
- **Only a stack being deleted loses its record.** CloudFormation also asks
  the cleanup to delete when it rolls back an update that added it, with the
  stack's VM still serving the name; the function reads the stack's status
  and keeps the record then. It also keeps it when a create rolls back in a
  zone you passed, where the name may be another stack's.
- **Give every stack in a shared zone its own `JitName`.** Two stacks with
  one name overwrite each other's record, and deleting either removes it.
  Changing `JitName` or the zone on an update leaves the old name's record
  behind; delete it by hand.
- **If the cleanup fails**, the stack stops in `DELETE_FAILED` and the
  event's reason names the function's log stream. Fix the cause and delete
  again.
- **What stays:** the cache bucket (`Cache=s3`; see above) and the SSM
  parameters under the prefix. The stack does not own the runner instances
  the VM launched: check for the owner's leftovers (`firerunner:owner` tag)
  before you delete a reaper stack.

## Reaper safety net (optional)

firerunner's own reaper runs on the control VM and checks with GitHub before
it ends anything. [`reaper.yaml`](reaper.yaml) is a backstop for when the
control VM is not there.

- **What it does.** A Lambda runs every `EveryMinutes` (default 15). It
  ends the install's instances (by `Owner`) whose latest start is more than
  `MaxAgeMinutes` ago (default 720). That covers running instances, and used
  instances left stopped.
- **Stopped pool instances (`ready`, `warming`) are kept.** It holds no pool
  key; the control VM ends pool instances whose MAC fails.
- **Spot requests.** It cancels a persistent spot request first. If that
  fails, it keeps the instance rather than leave the request open. A
  one-time request needs no cancel.
- **Errors.** One failure does not stop the sweep. The run then fails, so
  it shows in the function's error metrics.
- **It cannot ask GitHub whether a job is running.** Set the limit above
  your longest job.
- **`DryRun=true`** only logs what it would end.
- **Clean deletion.** The stack creates the function's log group itself, so
  deleting the stack deletes the logs too.

## Bring your own cloud: the cross-account role

[`byoc-role.yaml`](byoc-role.yaml) lets a firerunner control plane in
another AWS account, such as a hosted one, run runners in this account.

- **What it grants.** Exactly the runner-compute permissions of
  `control.yaml`'s role, and nothing else:
  - launch instances only tagged with this `Owner`, with their volumes,
    into this `VpcId`'s subnets and security groups;
  - start, re-tag, modify and terminate only instances with that tag;
  - cancel only that tag's spot requests;
  - read AMI parameters under the prefix, except `<prefix>/control/*`. A
    control VM of this account's own keeps its GitHub credentials and config
    there, so the role is denied them, even when both use the same prefix.

  [`test-policies.py`](test-policies.py) checks that the two copies are
  identical and that the role holds nothing more.
- **Modifying instances.** `ModifyInstanceAttribute` on the owner's
  instances is not limited to user-data. A holder could also change their
  instance type or termination protection. It reaches only instances that
  carry the owner tag.
- **Assuming it.** The trusted account must present the external ID, which
  is unique to your install. Set `TrustedRoleName` to accept only that one
  role in the account. The external ID guards against the confused deputy
  problem; it is not a secret, since the trust policy shows it.
- **Not yet supported.** Runners fetch their JIT config from an endpoint
  inside their VPC, identified by private IP. A control plane outside the
  VPC therefore also needs a JIT endpoint inside it, which this role does
  not provide yet.
