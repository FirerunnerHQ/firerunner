#!/usr/bin/env bash
# check-remote.sh — run scripts/check.sh on a Linux host, so the steps a
# macOS box skips (docker-shim, and vet-linux against a real Linux toolchain)
# fail here instead of in CI after a merge.
#
# The working tree, uncommitted changes included, is copied with rsync to a
# per-worktree directory on the host (.git and gitignored files excluded),
# check.sh runs there with its output streamed back, and the exit status is
# check.sh's. With --netns it then runs CI's netns job too (make netns-test
# and the separate-UID identity test), which need root, ip and nft; sudo is
# called as sudo -n, so it fails at once instead of waiting for a password.
#
# Usage: FR_REMOTE_HOST=user@host scripts/check-remote.sh [--fast] [--netns] [--clean]
#   --fast   pass --fast to check.sh (skip the race/coverage run)
#   --netns  also run the netns job (needs passwordless sudo on the host)
#   --clean  remove the remote copy and exit
#
# FR_REMOTE_HOST      ssh destination of a Linux host with go, git, make, rsync
# FR_REMOTE_DIR       copy location, relative to the remote home (default:
#                     .cache/firerunner-check-remote/<worktree>-<hash>)
# FR_REMOTE_DEADLINE  seconds for the whole run (default 1800); past it the
#                     run is stopped and the exit status is 124
# FR_REMOTE_GRACE     seconds the local watchdog waits past the deadline for
#                     the host's own timeout(1) to fire (default 30)
#
# Interrupting stops the local side only; the remote check runs on until it
# ends or its deadline.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)" || exit 2
cd "$ROOT" || exit 2

usage() { awk 'NR > 1 && !/^#/ { exit } NR > 1' "$0" | sed 's/^# \{0,1\}//'; }

# --- remote side: invoked by the local side inside the synced copy ----------
if [ "${1:-}" = --on-remote ]; then
	shift
	netns=0 args=()
	for arg in "$@"; do
		case "$arg" in
		--netns) netns=1 ;;
		*) args+=("$arg") ;;
		esac
	done
	# The copy has no .git: give check.sh an index of the copied files to
	# list, and tell it there is no history for the DCO step to read.
	if ! git init -q . || ! git add -A; then
		echo "check-remote: git init in the remote copy failed" >&2
		exit 2
	fi
	export FR_CHECK_NO_HISTORY=1 GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"
	rc=0
	scripts/check.sh ${args[@]+"${args[@]}"} || rc=$?
	[ "$netns" = 1 ] || exit "$rc"

	results=()
	echo
	echo "==> netns (sudo -n)"
	if ! sudo -n true 2>/dev/null; then
		echo "passwordless sudo is unavailable on this host (sudo -n true failed);"
		echo "the netns job needs root. Allow it in sudoers, or drop --netns."
		results+=("FAIL  netns  no passwordless sudo")
		rc=1
	else
		bin="$(mktemp -d)" || exit 2
		if go test -c -o "$bin/identity.test" ./internal/jobidentity &&
			sudo -n "$bin/identity.test" -test.run '^TestReaderSeparateUID$' -test.v; then
			results+=("PASS  identity-uid")
		else
			results+=("FAIL  identity-uid")
			rc=1
		fi
		rm -rf "$bin"
		if make --no-print-directory netns-test SUDO="sudo -n" FR_REQUIRE_NETNS=1; then
			results+=("PASS  netns")
		else
			results+=("FAIL  netns")
			rc=1
		fi
	fi
	echo
	echo "== netns summary"
	printf '%s\n' "${results[@]}"
	exit "$rc"
fi

# --- local side ---------------------------------------------------------------
clean=0 pass=()
for arg in "$@"; do
	case "$arg" in
	--fast | --netns) pass+=("$arg") ;;
	--clean) clean=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $arg" >&2
		exit 2
		;;
	esac
done

host="${FR_REMOTE_HOST:-}"
if [ -z "$host" ]; then
	echo "check-remote: set FR_REMOTE_HOST to the ssh destination of a Linux host" >&2
	exit 2
fi
deadline="${FR_REMOTE_DEADLINE:-1800}"
case "$deadline" in
'' | *[!0-9]*)
	echo "check-remote: FR_REMOTE_DEADLINE must be whole seconds, got '$deadline'" >&2
	exit 2
	;;
esac
# One directory per worktree, so two worktrees never share a copy.
sum="$(printf '%s' "$ROOT" | cksum | cut -d' ' -f1)"
dir="${FR_REMOTE_DIR:-.cache/firerunner-check-remote/$(basename "$ROOT")-$sum}"
# The directory is put into remote shell commands unquoted: allow only plain
# path characters, and nothing that could climb out of it.
case "$dir" in
'' | *[!A-Za-z0-9._/-]* | *..*)
	echo "check-remote: FR_REMOTE_DIR may use only letters, digits and . _ / - (and no ..): '$dir'" >&2
	exit 2
	;;
esac

ssh_opts=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30 -o ServerAliveCountMax=4)
start=$SECONDS
tmp="$(mktemp -d)" || exit 2
pid=
trap '[ -n "$pid" ] && kill "$pid" 2>/dev/null; rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# bounded <seconds> <command...>: run a command, stopping it after <seconds>.
# macOS has no timeout(1); this is a watchdog. Returns 124 when it fired.
bounded() {
	local limit="$1" rc
	shift
	rm -f "$tmp/fired"
	"$@" &
	pid=$!
	(
		n=0
		while [ "$n" -lt "$limit" ]; do
			sleep 1
			kill -0 "$pid" 2>/dev/null || exit 0
			n=$((n + 1))
		done
		: >"$tmp/fired"
		kill "$pid" 2>/dev/null
	) &
	local dog=$!
	wait "$pid"
	rc=$?
	pid=
	kill "$dog" 2>/dev/null
	wait "$dog" 2>/dev/null
	[ -e "$tmp/fired" ] && return 124
	return "$rc"
}
left() { echo $((deadline - (SECONDS - start))); }
expired() {
	echo "check-remote: deadline of ${deadline}s exceeded during $1 (FR_REMOTE_DEADLINE)" >&2
	exit 124
}

if [ "$clean" = 1 ]; then
	echo "==> removing $host:$dir"
	bounded "$(left)" ssh "${ssh_opts[@]}" "$host" "rm -rf -- $dir"
	rc=$?
	[ "$rc" = 124 ] && expired "clean"
	exit "$rc"
fi

echo "==> preparing $host:$dir"
bounded 60 ssh "${ssh_opts[@]}" "$host" "mkdir -p -- $dir"
rc=$?
if [ "$rc" != 0 ]; then
	if [ "$rc" = 124 ]; then
		echo "check-remote: ssh $host did not finish in 60s (is it waiting for an interactive login?)" >&2
	else
		echo "check-remote: cannot reach $host over ssh (exit $rc)" >&2
	fi
	exit "$rc"
fi

echo "==> syncing the working tree"
bounded "$(left)" rsync -az --delete --exclude=/.git --filter=':- .gitignore' \
	-e "ssh ${ssh_opts[*]}" ./ "$host:$dir/"
rc=$?
[ "$rc" = 124 ] && expired "sync"
[ "$rc" = 0 ] || {
	echo "check-remote: rsync to $host failed (exit $rc; is rsync installed there?)" >&2
	exit "$rc"
}

# The host's timeout(1) stops the remote processes at the deadline; the local
# watchdog, a little later, covers a host without it or a dead connection.
remaining="$(left)"
[ "$remaining" -gt 0 ] || expired "sync"
echo "==> scripts/check.sh ${pass[*]:-} on $host (deadline ${remaining}s)"
# Single-quoted for whatever the remote login shell is; no ' inside.
run="bash scripts/check-remote.sh --on-remote ${pass[*]:-}"
cmd="bash -c 'cd -- $dir || exit 2; if command -v timeout >/dev/null; then exec timeout -k 15 $remaining $run; fi; exec $run'"
bounded $((remaining + ${FR_REMOTE_GRACE:-30})) ssh "${ssh_opts[@]}" "$host" "$cmd"
rc=$?
[ "$rc" = 124 ] && expired "the remote check"
exit "$rc"
