#!/usr/bin/env bash
# Tests for check-remote.sh with ssh, rsync, sudo, make and go stubbed: ssh
# runs its command in a local directory standing in for the remote home, and
# rsync copies into it with the real rsync, so the exclusions are the real
# ones. The worktree is a fixture whose scripts/check.sh is a stub that
# records how it was called. Needs bash, git and rsync; no network.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

REAL_RSYNC="$(command -v rsync)" || {
	echo "FAIL rsync is not installed"
	exit 1
}
export REAL_RSYNC
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$TMP/gitconfig"
: >"$GIT_CONFIG_GLOBAL"
unset FR_REMOTE_DIR FR_REMOTE_DEADLINE FR_CHECK_NO_HISTORY

fail=0
check() { # check <name> <command...>
	local name="$1"
	shift
	if "$@"; then echo "ok   $name"; else
		echo "FAIL $name"
		[ -e "$TMP/out" ] && sed 's/^/     | /' "$TMP/out"
		fail=1
	fi
}
# Called through check, which shellcheck cannot see.
# shellcheck disable=SC2329
has() { grep -qF -- "$1" "$OUT"; }
# shellcheck disable=SC2329
lacks(){ ! grep -qF -- "$1" "$OUT"; }

STUB="$TMP/stub"
REMOTE="$TMP/remote" # the remote home
export STUB REMOTE
mkdir -p "$STUB" "$REMOTE"

# ssh [-o opt]... host command: log, then run the command in the remote home.
cat >"$STUB/ssh" <<'EOF'
#!/usr/bin/env bash
while [ "$1" = -o ]; do shift 2; done
host="$1"
shift
printf '%s\t%s\n' "$host" "$*" >>"$STUB/ssh.log"
[ -e "$STUB/ssh-hang" ] && case "$*" in *on-remote*) exec sleep 300 ;; esac
cd "$REMOTE" && exec bash -c "$*"
EOF
# rsync ... src host:dir/: the real rsync, into the remote home. openrsync
# (macOS) runs "rsync --server" from PATH for its other end: pass that through.
cat >"$STUB/rsync" <<'EOF'
#!/usr/bin/env bash
case " $* " in *" --server "*) exec "$REAL_RSYNC" "$@" ;; esac
args=()
while [ $# -gt 2 ]; do
	if [ "$1" = -e ]; then shift 2; continue; fi
	args+=("$1")
	shift
done
printf '%s\n' "$2" >>"$STUB/rsync.log"
exec "$REAL_RSYNC" "${args[@]}" "$1" "$REMOTE/${2#*:}"
EOF
# sudo: refuses without -n (a real one would prompt); with sudo-denied, -n fails.
cat >"$STUB/sudo" <<'EOF'
#!/usr/bin/env bash
[ "$1" = -n ] || { echo "sudo would prompt for a password"; exit 99; }
shift
[ -e "$STUB/sudo-denied" ] && { echo "sudo: a password is required" >&2; exit 1; }
echo "sudo $*" >>"$STUB/sudo.log"
exec "$@"
EOF
cat >"$STUB/make" <<'EOF'
#!/usr/bin/env bash
echo "make $*" >>"$STUB/make.log"
EOF
# go test -c -o FILE ...: a test binary that passes.
cat >"$STUB/go" <<'EOF'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do [ "$1" = -o ] && printf '#!/bin/sh\necho identity-ran\n' >"$2" && chmod +x "$2"; shift; done
EOF
# timeout -k N SECS cmd...: log the deadline, run the command.
cat >"$STUB/timeout" <<'EOF'
#!/usr/bin/env bash
echo "timeout $1 $2 $3" >>"$STUB/timeout.log"
shift 3
exec "$@"
EOF
chmod +x "$STUB"/*
export PATH="$STUB:$PATH"

# The worktree: check-remote.sh under test, a stub check.sh, a worktree-style
# .git file, an ignored build output and an uncommitted file.
WT="$TMP/wt"
mkdir -p "$WT/scripts"
cp "$ROOT/scripts/check-remote.sh" "$WT/scripts/"
cat >"$WT/scripts/check.sh" <<'EOF'
#!/usr/bin/env bash
echo "stub-check args=[$*] no_history=${FR_CHECK_NO_HISTORY:-} gofiles=$(git ls-files | tr '\n' ' ')"
exit "$(cat rc 2>/dev/null || echo 0)"
EOF
chmod +x "$WT/scripts/check.sh"
echo "gitdir: /elsewhere" >"$WT/.git"
printf '%s\n' '*.out' '/firerunner' >"$WT/.gitignore"
echo x >"$WT/cover.out"
echo x >"$WT/firerunner"
echo 'package main' >"$WT/new.go"

DIR=".cache/firerunner-check-remote/wt-$(printf '%s' "$WT" | cksum | cut -d' ' -f1)"
COPY="$REMOTE/$DIR"

# run [args...]: check-remote.sh in the worktree; output in $OUT, status in $RC.
run() {
	OUT="$TMP/out"
	"$WT/scripts/check-remote.sh" "$@" >"$OUT" 2>&1 && RC=0 || RC=$?
}

# 1. No host: a usage error, nothing contacted.
FR_REMOTE_HOST='' run
check "no host: exit 2" [ "$RC" = 2 ]
check "no host: says what to set" has "set FR_REMOTE_HOST"
check "no host: no ssh" [ ! -e "$STUB/ssh.log" ]

export FR_REMOTE_HOST=dev@linux-box

# 2. A run: synced, check.sh streamed, its status returned.
run --fast
check "run: exit 0" [ "$RC" = 0 ]
check "run: ssh to the host" grep -q '^dev@linux-box' "$STUB/ssh.log"
check "run: per-worktree dir" grep -qxF "dev@linux-box:$DIR/" "$STUB/rsync.log"
check "run: uncommitted file synced" [ -e "$COPY/new.go" ]
check "run: ignored output not synced" [ ! -e "$COPY/cover.out" ]
check "run: ignored binary not synced" [ ! -e "$COPY/firerunner" ]
check "run: .git not synced" [ -d "$COPY/.git" ]
check "run: check.sh output streamed" has "stub-check args=[--fast] no_history=1"
check "run: copy has an index" has "new.go"
check "run: remote deadline via timeout" grep -q '^timeout -k 15 [0-9]' "$STUB/timeout.log"

# 3. The remote status is the exit status; files deleted locally go remotely.
echo 3 >"$WT/rc"
rm "$WT/new.go"
run
check "status: exit 3" [ "$RC" = 3 ]
check "status: deleted file removed" [ ! -e "$COPY/new.go" ]
rm "$WT/rc"

# 4. --netns without passwordless sudo fails at once, naming the reason.
: >"$STUB/sudo-denied"
run --netns
check "no sudo: exit 1" [ "$RC" = 1 ]
check "no sudo: says why" has "passwordless sudo is unavailable"
check "no sudo: summary line" has "FAIL  netns  no passwordless sudo"
check "no sudo: netns not run" [ ! -e "$STUB/make.log" ]
check "no sudo: never prompts" lacks "would prompt"
rm "$STUB/sudo-denied"

# 5. --netns with it: CI's netns job, sudo -n throughout.
run --netns
check "netns: exit 0" [ "$RC" = 0 ]
check "netns: check.sh ran first" has "stub-check args=[]"
check "netns: identity test as root" grep -q 'identity.test -test.run' "$STUB/sudo.log"
check "netns: make netns-test with sudo -n" grep -qF 'make --no-print-directory netns-test SUDO=sudo -n FR_REQUIRE_NETNS=1' "$STUB/make.log"
check "netns: summary" has "PASS  netns"

# 6. A hung remote is stopped at the deadline with status 124.
: >"$STUB/ssh-hang"
start=$SECONDS
FR_REMOTE_DEADLINE=2 FR_REMOTE_GRACE=1 run
check "deadline: exit 124" [ "$RC" = 124 ]
check "deadline: says so" has "deadline of 2s exceeded"
check "deadline: bounded" [ $((SECONDS - start)) -lt 20 ]
rm "$STUB/ssh-hang"

# 7. A directory that could escape the remote home is refused.
FR_REMOTE_DIR='../x; rm -rf ~' run
check "bad dir: exit 2" [ "$RC" = 2 ]
check "bad dir: says why" has "FR_REMOTE_DIR may use only"

# 8. --clean removes the copy.
run --clean
check "clean: exit 0" [ "$RC" = 0 ]
check "clean: copy removed" [ ! -e "$COPY" ]

# 9. Unknown arguments are refused.
run --bogus
check "unknown arg: exit 2" [ "$RC" = 2 ]

exit "$fail"
