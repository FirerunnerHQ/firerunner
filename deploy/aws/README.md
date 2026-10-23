# Control plane on AWS

[`control.yaml`](control.yaml) runs firerunner's control plane for EC2 tiers
as one VM in an Auto Scaling group of one:

- **Replacement on failure.** The group replaces a VM that fails EC2's
  checks, one whose boot fails (bad release, missing parameter, rejected
  config), and one whose firerunner unit fails five times in ten minutes.
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

1. User-data reads `<prefix>/control/version` (a release tag, e.g.
   `v0.2.0`). It downloads that release's `SHA256SUMS`, the binary for its
   architecture and `firerunner-control-bootstrap.sh`, and checks both
   files against `SHA256SUMS`.
2. [`control-bootstrap.sh`](control-bootstrap.sh) installs the binary and
   writes `/etc/firerunner/` from SSM. Then `firerunner config check` must
   pass, and only then does the name move and the unit start.
3. Any failure marks the instance unhealthy. The group replaces it, and the
   log says why (see [Debugging](#debugging)).

## Before the first deploy

You need:

- A VPC whose subnets have egress to GitHub. DNS support and DNS hostnames
  must be on, for the private zone.
- The runners' security group.
- A runner AMI published to an SSM parameter under the prefix
  (see [`images/`](../../images/)).
- A release of firerunner: a `v*` tag publishes one.

Put the config in SSM, under the prefix (default `/firerunner`):

| parameter | type | holds |
|---|---|---|
| `<prefix>/control/env` | String | non-secret `FR_*` lines: `FR_URL`, `FR_APP_CLIENT_ID`, `FR_APP_INSTALLATION_ID`, tuning flags |
| `<prefix>/control/tiers` | String | the tier catalog (JSON); `ami` may name `<prefix>/...` parameters |
| `<prefix>/control/app-private-key` | SecureString | the GitHub App key (or `token`: a PAT) |
| `<prefix>/control/version` | String | written by `deploy-control.sh` |

The bootstrap sets `FR_OWNER`, `FR_JIT_ADDR`, `FR_JIT_HOST`,
`FR_AWS_REGION`, `FR_TIERS` and `FR_WORKDIR` itself; leave them out of
`env`.

```sh
aws ssm put-parameter --name /firerunner/control/env --type String --value "$(cat env)"
aws ssm put-parameter --name /firerunner/control/tiers --type String --value "$(cat tiers.json)"
aws ssm put-parameter --name /firerunner/control/app-private-key --type SecureString --value "$(cat app.pem)"
aws ssm put-parameter --name /firerunner/control/version --type String --value v0.2.0
```

Create the stack. Pick an `Owner` that names this install and **never
change it**: the owner tag is how a replacement VM finds the pool.

```sh
aws cloudformation deploy --stack-name firerunner --template-file deploy/aws/control.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides \
  Owner=acme-prod VpcId=vpc-... SubnetIds=subnet-a,subnet-b \
  RunnerSecurityGroupId=sg-...
```

Pass `HostedZoneId=Z...` with `ZoneName=<its name>` to use an existing
private zone. Use `ImageId=/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64`
with an x86 `InstanceType`.

## Deploy, roll back

```sh
deploy/aws/deploy-control.sh firerunner v0.2.1   # waits until v0.2.1 is active
deploy/aws/deploy-control.sh firerunner v0.2.0   # roll back: the same, older
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

A config the VM rejects makes the group replace it again and again, about
every five minutes, until the parameter is fixed. Fix it, then deploy.
