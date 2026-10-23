# scripts/e2e — end-to-end checks against a real deployment

Small scripts for verifying an EC2 (or Firecracker) deployment end to end, as
the README's [*Verifying a deployment end to
end*](../../README.md#ec2-tiers-provisioner-ec2) describes. They wrap the `aws`
and `gh` calls such a check needs, with deadlines, so a person or an agent
gets an answer, not a hung terminal.

**These touch real resources.** `dispatch-and-measure.sh` starts workflow runs,
which launch billable runner instances, and so does `lifecycle.sh` without `--run`. `remote-exec.sh` runs commands as root on
an instance. The others only read.

| script | does |
|---|---|
| `env.sh` | shared configuration; run it directly to see what is set or missing |
| `remote-exec.sh [INSTANCE] -- CMD…` | runs a command through SSM, prints its output, exits with its status |
| `console.sh [INSTANCE] [-n LINES]` | prints the tail of an instance's serial console |
| `watch-run.sh RUN_ID` | waits for a workflow run and fails as soon as a job fails or none starts |
| `dispatch-and-measure.sh [WORKFLOW] N [-f K=V]…` | dispatches N runs and reports each job's queue and run seconds |
| `lifecycle.sh [WORKFLOW] N` or `--run RUN_ID…` | each job's runner instance from launch to terminated, sampled every `-i` seconds (1), e.g. job end to shutting-down |

Each script has `--help`. Exit statuses are shared: 0 success, 1 the thing
under test failed, 2 usage or configuration, 3 no job started in time, 124 a
deadline passed. Results meant for a program are JSON lines on stdout. Progress
and hints go to stderr.

## Configuration

Only environment variables, and none has a default that names an account, a
host or a repository. Export them once per shell:

```bash
export AWS_REGION=…            # region of the control VM and the runners
export AWS_PROFILE=…           # optional; otherwise the default credential chain
export FR_E2E_REPO=OWNER/REPO  # where the e2e workflows run
export FR_E2E_CONTROL_INSTANCE=i-…   # optional default for remote-exec and console
export FR_E2E_WORKFLOW=e2e-selftest.yml  # optional default for dispatch-and-measure
scripts/e2e/env.sh             # check
```

The `aws` CLI reads `AWS_PROFILE` and `AWS_REGION` itself, so no script builds
`--profile`/`--region` flags. Building them in a variable such as
`A="--profile x"; aws $A` breaks in zsh, which does not split words.

Optional tuning: `FR_E2E_REF` (the branch to dispatch on),
`FR_E2E_START_DEADLINE` (300 s for a job to start), `FR_E2E_RUN_DEADLINE`
(1800 s for a run to finish), `FR_E2E_EXEC_DEADLINE` (300 s for a remote
command) and `FR_E2E_POLL` (5 s).

## Credentials

Three separate credentials are involved, and none of them lives in this
repository:

- **AWS:** a profile or role in the deployment's account. It needs
  `ssm:SendCommand`, `ssm:GetCommandInvocation` and `ssm:CancelCommand` on the
  control VM, and `ec2:GetConsoleOutput` and `ec2:DescribeInstances` on the
  runners. `remote-exec.sh` also needs the control VM to run the SSM agent with an
  instance role that allows it, for example `AmazonSSMManagedInstanceCore`.
- **GitHub, for `gh`:** your own `gh auth login`, with the Actions read and
  write permission on `FR_E2E_REPO`. Dispatching needs write access.
- **GitHub, for firerunner:** the GitHub App private key that the deployment
  under test uses (`FR_APP_PRIVATE_KEY`). It is installed on the control VM,
  not on the machine that runs these scripts. The harness never needs it. If
  you have to run firerunner yourself, ask the deployment's operator for it,
  and never copy it into a repository, an issue or a log.

## A typical session

```bash
# 1. Is the control VM's firerunner up, and what does it see?
scripts/e2e/remote-exec.sh -- firerunner status --json
scripts/e2e/remote-exec.sh -- journalctl -u firerunner -n 100 --no-pager

# 2. One run, watched to the end (exits early on failure or no runner).
gh workflow run e2e-selftest.yml --repo "$FR_E2E_REPO"
run_id=$(gh run list --repo "$FR_E2E_REPO" --workflow e2e-selftest.yml --limit 1 --json databaseId --jq '.[0].databaseId')
scripts/e2e/watch-run.sh "$run_id"

# 3. No job started (exit 3)? Find the runner instance and read its console.
aws ec2 describe-instances --filters Name=tag-key,Values=firerunner:tier \
  --query 'Reservations[].Instances[].[InstanceId,State.Name,LaunchTime]' --output text
scripts/e2e/console.sh i-… -n 200

# 4. Launch latency: 5 runs, per-job queue seconds and a p50/max summary.
scripts/e2e/dispatch-and-measure.sh e2e-selftest.yml 5
```

`remote-exec.sh` arguments arrive on the instance exactly as given. Each one is
quoted, so a presigned URL's `&` or a `$` survives. To use pipes, pass a
single argument, which runs as a shell script, or pipe a script on stdin. The
instance kills the command at the deadline. Still bound commands that never
end yourself, such as `tcpdump -c 50` or `timeout 30 …`, so that their output
is kept.

## Tests

`scripts/e2e/test-e2e.sh` runs every script against fake `aws`, `gh`, `date`
and `sleep`. It needs no account, no token and no real waiting, and runs on
macOS's bash 3.2. `make check` runs it, and so does CI's test job.
