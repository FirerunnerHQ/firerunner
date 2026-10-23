# AGENTS.md

Working notes for coding agents (and people) changing firerunner. The README
explains what firerunner is; this file explains how to change it safely.

## Before you push

```bash
make check                      # everything CI's test job checks, with a PASS/FAIL/SKIP summary
make check CHECK_ARGS=--fast    # same, without the slow race/coverage run
```

A `SKIP` line always says why; for example, `docker-shim` needs Linux. CI also
runs jobs `make check` cannot: `netns` (root, nftables), `cache-protocol` (npm)
and `e2e` (a KVM host).

Narrower loops:

```bash
go test -race ./internal/ec2/...          # one package
GOOS=linux go vet ./...                   # the Firecracker code is Linux-only
firerunner config check --catalog FILE    # validate a tier catalog offline
```

## Commits and pull requests

- **Sign off every commit** with `git commit -s`. The `dco` workflow rejects a PR
  with any commit lacking a `Signed-off-by` that matches its author email.
  `make check` checks it locally; to fix a branch, run
  `git rebase --signoff origin/main`.
- One topic per commit. Use the subject style `pkg: what changed`, in the
  imperative, with a body that says why. Look at `git log` for the house style.
- One issue per PR, linked with `Closes #N`. Every behaviour change ships with
  tests.
- Merge with a merge commit (`gh pr merge --merge --delete-branch`).
- **Stacked PRs:** before merging a PR that another PR is based on, retarget the
  child to `main` first (`gh pr edit <child> --base main`). Deleting a base
  branch closes, rather than retargets, the PRs on it.
- A PR merged into a stack branch, not `main`, does not close its `Closes #N`
  issue. Close it by hand when the stack lands.
- `scripts/merge-stack.sh PR` does both: it prints the plan (retarget, merge,
  close), and `--yes` carries it out.

## Public repository hygiene

This repository is public. Never commit, or write in issues or PRs:

- cloud account IDs, VPC, subnet or security group IDs, bucket names;
- private host names or customer and organisation names;
- prices, costs or margins.

Placeholders such as `subnet-0aaa…` are fine.

## Testing norms

- **No real cloud in unit tests.** AWS calls go through the narrow interfaces in
  `internal/ec2/api.go`. Use the in-memory fake in `internal/ec2/fake_test.go`,
  which scripts errors per instance type, subnet and market, and tracks
  instance states.
- **No timing guesses.** Wait on a condition (`eventually` helpers, channels),
  never `time.Sleep` followed by an assertion. A test that can pass without
  observing anything is a bug.
- **Prove a test can fail.** For logic that guards money or security (capacity
  walks, termination, JIT handoff, error classification), break the code on
  purpose once and watch a test fail.
  `scripts/mutate.sh FILE 'old' 'new'` does it and always restores the file;
  it ends in `CAUGHT`, `SURVIVED` or `INVALID` (see its header).
- Shell scripts under `images/` have shell tests (`images/test-*.sh`) that
  stub external commands. Keep them shellcheck-clean.

## Known pitfalls

- **`scaleset.Client` serialises every call.** In `actions/scaleset` v0.4.0 its
  `mu` is held across the whole HTTP request and is not context-aware, so a
  context deadline does not bound a call while another goroutine holds the
  client. Give each concurrent use its own client: `internal/listener` keeps
  separate ones for message sessions, runner removal and a pool for JIT
  generation. Do not add a second user to an existing client.
- **Persistent spot requests: cancel after the start.** A stopped instance
  whose persistent request was cancelled can never start again
  (`IncorrectSpotRequestState`), and terminating one whose request is still
  open makes AWS launch a replacement. So the stopped pool starts, then
  cancels, then may terminate (`internal/ec2/provisioner.go`). Persistent
  requests also reject terminate-on-shutdown; one-time spot accepts it. The
  fake in `internal/ec2/fake_test.go` encodes these rules (`StartInstances`,
  `termBeforeCancel`); if you learn a new AWS rule, add it there first.
- **Changing a flag's or option's meaning:** sweep `README.md`,
  `config.example.env`, `deploy/` and code comments in the same commit, e.g.
  `git grep -n -e '--max-runners' -e FR_MAX_RUNNERS`. Stale mentions found one
  per review round cost one PR five rounds.
- **Secrets are never flag defaults.** The flag package prints defaults in
  `-h` and on every parse error. `internal/config` registers `--token` and
  `--app-private-key` with an empty default and reads `FR_TOKEN` /
  `FR_APP_PRIVATE_KEY` after parsing; follow that for any new credential.

## Shell and test hygiene for agents

The usual dev box is macOS with zsh; much of the code and CI is Linux.

- **zsh globs.** An unmatched glob aborts the command: `grep -r --include=*.go
  x .` prints "no matches found" and exits 1, which looks like an empty
  result. Quote globs, or use `git grep -n x -- '*.go'` or `rg -g '*.go'`. zsh
  also does not word-split `$VAR`; use arrays.
- **Pipes hide failures.** `go test ./... | tail` exits with `tail`'s status;
  a failing test was committed that way. Use `set -o pipefail`, or check
  `${pipestatus[1]}` (zsh) / `${PIPESTATUS[0]}` (bash).
- **Always pass `-timeout`** (e.g. `go test -timeout 120s ./internal/ec2/...`).
  The default is 10 minutes, longer than an agent's tool call limit, so a hang
  is reported as a tool timeout rather than a goroutine dump.
- **BSD userland.** macOS has no `timeout`, and `sed -i` needs `''`. The
  shell tests under `images/` run on both macOS and Linux in `make check`, so
  keep them portable.
- **Linux-only files** (`_linux.go`, `//go:build linux`) are skipped by a
  darwin build, so a green `go vet` there proves nothing about them: run
  `GOOS=linux go vet ./...` too.

## Where things are

| path | what |
|---|---|
| `cmd/firerunner` | daemon wiring, the `status`/`doctor`/`config`/`cache-server` subcommands |
| `internal/config` | flags, `FR_*` env, the tier catalog and its validation |
| `internal/scheduler` | per-tier launch and scale-down; `Launch` blocks for a runner's lifetime |
| `internal/listener` | GitHub scale-set sessions, with per-tier reconnect |
| `internal/provisioner` | Firecracker microVMs, Linux only |
| `internal/ec2` | one EC2 instance per job; `internal/jitserver` serves its JIT config once over the VPC |
| `internal/health` | per-tier listener state, published for `status`/`doctor` |
| `internal/diag` | `status` and `doctor` |
| `images/` | guest images, boot scripts and their tests |

## Operating a deployment

`firerunner status --json`, `firerunner doctor --json` and
`firerunner config check --json` are the machine-readable views. Prefer them to
parsing logs. When something an agent needs is missing from them, add it
there.
