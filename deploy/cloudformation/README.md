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
   asset replaced afterwards is refused.
2. [`control-bootstrap.sh`](control-bootstrap.sh) installs the binary and
   writes `/etc/firerunner/` from SSM. Then `firerunner config check` must
   pass, and only then does the name move and the unit start.
3. Any failure marks the instance unhealthy. The group replaces it, and the
   log says why (see [Debugging](#debugging)).

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

## Reaper safety net (optional)

firerunner's own reaper runs on the control VM and checks with GitHub before
it ends anything. [`reaper.yaml`](reaper.yaml) is a backstop for when the
control VM is not there.

- **What it does.** A Lambda runs every `EveryMinutes` (default 15). It
  ends the install's instances (by `Owner`) whose latest start is more than
  `MaxAgeMinutes` ago (default 720). That covers running instances, and used
  instances left stopped.
- **Stopped pool instances (`ready`, `warming`) are kept.**
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
  - read AMI parameters under the prefix.

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
