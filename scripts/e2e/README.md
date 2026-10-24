# scripts/e2e — end-to-end checks against a real deployment

Small scripts for verifying an EC2 (or Firecracker) deployment end to end, as
the README's [*Verifying a deployment end to
end*](../../README.md#ec2-tiers-provisioner-ec2) describes. They wrap the `aws`
and `gh` calls such a check needs, with deadlines, so a person or an agent
gets an answer, not a hung terminal. Use them rather than raw `aws` calls: they
know which resources are yours.

**These touch real resources.** `control-vm.sh up` creates a stack and a VM.
`dispatch-and-measure.sh` starts workflow runs, which launch billable runner
instances, and so does `lifecycle.sh` without `--run`. `remote-exec.sh` runs
commands as root on an instance. `inventory.sh --cleanup --yes` and
`control-vm.sh down` delete things. The others only read.

| script | does |
|---|---|
| `env.sh` | shared configuration; run it directly to see what is set, your owner, and who else is using the environment |
| `control-vm.sh up\|status\|install FILE\|down\|list` | your own control VM: a stack made from [`deploy/cloudformation/control.yaml`](../../deploy/cloudformation/control.yaml) |
| `inventory.sh [--owner O] [--cleanup [--yes]]` | an owner's instances, spot requests and volumes; cleanup of your own |
| `remote-exec.sh [INSTANCE] -- CMD…` | runs a command through SSM, prints its output, exits with its status |
| `console.sh [INSTANCE] [-n LINES]` | prints the tail of an instance's serial console |
| `watch-run.sh RUN_ID` | waits for a workflow run and fails as soon as a job fails or none starts; saves the failed runner's console |
| `dispatch-and-measure.sh [WORKFLOW] N [-f K=V]…` | dispatches N runs and reports each job's queue and run seconds |
| `lifecycle.sh [WORKFLOW] N` or `--run RUN_ID…` | each job's runner instance from launch to terminated, sampled every `-i` seconds (1), e.g. job end to shutting-down |

Each script has `--help`. Exit statuses are shared: 0 success, 1 the thing
under test failed, 2 usage or configuration, 3 no job started in time, 124 a
deadline passed. Results meant for a program are JSON lines on stdout; the
last line is the summary. Progress and hints go to stderr.

## The shared environment: rules

Several sessions, people or agents, may use one AWS account and one GitHub
organisation for e2e at the same time. On 2026-10-23 one session terminated a
pool instance that another session's control VM had adopted two minutes
earlier. So:

1. **Your own owner.** Every session runs its control VM with its own
   `FR_OWNER`. firerunner tags every instance, volume and spot request it
   creates `firerunner:owner=<FR_OWNER>`, and its reaper (`--ec2-reaper`)
   touches only its own owner's. `env.sh` derives one per worktree:
   `e2e-<worktree name>-<hash>`, or `e2e-<branch>-<hash>` outside a worktree,
   never from `main`. The hash covers the worktree's full path, or the branch,
   and the host. So two worktrees whose names share a prefix, or one branch on
   two machines, never share an owner. Owners are lower case. Set `FR_OWNER`
   to reuse an older owner and adopt its pool.
2. **Your own tier names.** A tier's name is its scale set's name. Two control
   VMs on one name share one scale set and take each other's jobs.
   `control-vm.sh up` suffixes every tier name and label with
   `-FR_E2E_TIER_SUFFIX` (default: your owner without `e2e-`); use those in
   `runs-on`.
3. **Clean up only your own owner's resources.** Prefer the reaper. Otherwise
   use `control-vm.sh down` or `inventory.sh --cleanup`, which refuse any owner
   but yours, and refuse while that owner's control VM is live, or while a
   control VM's owner cannot be read. Both refuse the shared stack's own
   owner, and refuse a session stack that another owner made. Never
   terminate an instance or cancel a spot request by hand unless you have
   checked its `firerunner:owner` tag.
4. **Look before you touch.** `scripts/e2e/env.sh` and
   `scripts/e2e/control-vm.sh list` show the live control VMs and their
   owners. A VM whose owner shows as `unknown` was made by hand: ask before
   assuming it is abandoned.

A session's own stack scopes its VM's role to its owner (see
[`deploy/cloudformation/control.yaml`](../../deploy/cloudformation/control.yaml)), so even a
misconfigured firerunner cannot terminate another owner's instances.

## Configuration

Environment variables, or a gitignored `.claude/e2e.env` of `KEY=VALUE`
lines. No default names an account, a host or a repository:

```bash
# .claude/e2e.env in the main checkout: read by every worktree of it
AWS_REGION=…              # region of the control VMs and the runners
AWS_PROFILE=…             # optional; otherwise the default credential chain
FR_E2E_REPO=OWNER/REPO    # where the e2e workflows run
FR_E2E_STACK=…            # the shared deploy/cloudformation/control.yaml stack (control-vm.sh)
FR_E2E_BUCKET=…           # optional: for control-vm.sh install
```

`scripts/e2e/env.sh` checks it. The file is read at every call, so it works in
any shell. Order of precedence: the environment, `$FR_E2E_ENV_FILE`, this
checkout's `.claude/e2e.env`, then the main checkout's, which serves every
agent worktree under `.claude/worktrees/`. Only `AWS_PROFILE`, `AWS_REGION`
and `FR_*` lines count, and their values are literal, with no expansion. The
main checkout's file never sets `FR_OWNER` or `FR_E2E_TIER_SUFFIX`, not even
for the main checkout: those are per session. Export them, or put them in a
worktree's own file. In Claude Code, the SessionStart hook
(`scripts/agent-env.sh`) also exports the file's values. So raw `aws` calls
get the same `AWS_PROFILE` and `AWS_REGION` without an export in every Bash
call. After you edit the file, start a new session, or run
`eval "$(scripts/e2e/env.sh --exports)"`.

The `aws` CLI reads `AWS_PROFILE` and `AWS_REGION` itself, so no script builds
`--profile`/`--region` flags. Building them in a variable such as
`A="--profile x"; aws $A` breaks in zsh, which does not split words.

Optional: `FR_OWNER`, `FR_E2E_TIER_SUFFIX`, `FR_E2E_VERSION` (the release
`control-vm.sh up` runs; default: the shared stack's), `FR_E2E_ARTIFACTS`
(where `watch-run.sh` saves consoles; default
`$TMPDIR/firerunner-e2e/<owner>`), `FR_E2E_CONTROL_INSTANCE` (default target
of `remote-exec.sh` and `console.sh`), `FR_E2E_WORKFLOW` and `FR_E2E_REF` (for
`dispatch-and-measure.sh`). Deadlines: `FR_E2E_START_DEADLINE` (300 s for a
job to start), `FR_E2E_RUN_DEADLINE` (1800 s for a run to finish),
`FR_E2E_EXEC_DEADLINE` (300 s for a remote command), `FR_E2E_STACK_DEADLINE`
(1200 s for a control VM to come up or go) and `FR_E2E_POLL` (5 s).

## Your control VM

`control-vm.sh up` creates the stack `FR_E2E_STACK-FR_OWNER` from
`deploy/cloudformation/control.yaml`. It has the shared stack's network and instance
parameters, `Owner=FR_OWNER`, and the JIT name `<FR_OWNER>.<shared zone>`.
Its configuration lives under `<shared prefix>/sessions/<FR_OWNER>/`: the
shared `env` and app key, the shared tier catalog (or `--tiers FILE`) with
your suffix, any runner AMI parameter the catalog names, and the release. It
waits until firerunner is active on the VM and prints the session as JSON,
tier names included. Running `up` again only waits.

`control-vm.sh install FILE` swaps in a development build (`GOOS=linux`, the
VM's architecture). It goes up to `FR_E2E_BUCKET` and the VM fetches it
through a presigned URL. A VM that the group replaces boots the release again.

`control-vm.sh down` deletes the stack first, so that nothing relaunches
runners, then runs `inventory.sh --cleanup --yes`. Then it deletes the JIT
name and the session's parameters. The runner scale sets stay registered on
GitHub; the next `up` with the same owner reuses them.

The shared stack itself, made and deployed as
[`deploy/cloudformation/README.md`](../../deploy/cloudformation/README.md) describes, is never
changed by these scripts.

## Credentials

Three separate credentials are involved, and none of them lives in this
repository:

- **AWS:** a profile or role in the deployment's account. A profile can live in
  `~/.aws/credentials` (`[name]`) or `~/.aws/config` (`[profile name]`);
  `env.sh` says which file holds `AWS_PROFILE`, or that neither does. Reading needs
  `ec2:Describe*`, `ec2:GetConsoleOutput`, `ssm:SendCommand`,
  `ssm:GetCommandInvocation`, `ssm:CancelCommand` and
  `cloudformation:DescribeStacks`. `control-vm.sh` needs, in addition, rights
  to create and delete the stack's resources (CloudFormation, IAM role, EC2
  launch template and security group, Auto Scaling, Route 53 records) and to
  read and write the SSM parameters under the prefix. Cleanup needs
  `ec2:CancelSpotInstanceRequests`, `ec2:TerminateInstances` and
  `ec2:DeleteVolume`. `remote-exec.sh` needs the instance to run the SSM agent
  with a role that allows it; the control VM's does.
- **GitHub, for `gh`:** your own `gh auth login`, with the Actions read and
  write permission on `FR_E2E_REPO`. Dispatching needs write access.
- **GitHub, for firerunner:** the GitHub App private key that the deployment
  under test uses. It lives in the shared stack's SSM parameters, and
  `control-vm.sh up` copies it, as a SecureString, without passing it on a
  command line. Never copy it into a repository, an issue or a log.

## A typical session

```bash
# 0. Who is here, and what am I?
scripts/e2e/env.sh

# 1. My control VM: prints JSON with the instance and my tier names.
scripts/e2e/control-vm.sh up
scripts/e2e/control-vm.sh install ./firerunner-linux-arm64   # optional dev build
scripts/e2e/remote-exec.sh i-… -- firerunner status --json

# 2. One run, watched to the end (exits early on failure or no runner, and
#    saves the failed runner's console first).
gh workflow run e2e-selftest.yml --repo "$FR_E2E_REPO"
run_id=$(gh run list --repo "$FR_E2E_REPO" --workflow e2e-selftest.yml --limit 1 --json databaseId --jq '.[0].databaseId')
scripts/e2e/watch-run.sh "$run_id"

# 3. What do I have running?
scripts/e2e/inventory.sh

# 4. Launch latency: 5 runs, per-job queue seconds and a p50/max summary.
scripts/e2e/dispatch-and-measure.sh e2e-selftest.yml 5

# 5. Done: the stack, my runners, my JIT name and my parameters go.
scripts/e2e/control-vm.sh down
```

`remote-exec.sh` arguments arrive on the instance exactly as given. Each one is
quoted, so a presigned URL's `&` or a `$` survives. To use pipes, pass a
single argument, which runs as a shell script, or pipe a script on stdin. The
instance kills the command at the deadline. Still bound commands that never
end yourself, such as `tcpdump -c 50` or `timeout 30 …`, so that their output
is kept.

## Tests

`scripts/e2e/test-e2e.sh` runs every script against fake `aws`, `gh`, `curl`,
`date` and `sleep`. It needs no account, no token and no real waiting, and
runs on macOS's bash 3.2. `make check` runs it, and so does CI's test job.
