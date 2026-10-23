#!/usr/bin/env bash
# remote-exec.sh — run a command on an instance through SSM and wait for it.
#
# Usage:
#   scripts/e2e/remote-exec.sh [INSTANCE] [--deadline S] -- CMD [ARG...]
#   scripts/e2e/remote-exec.sh [INSTANCE] [--deadline S] -- 'SHELL SCRIPT'
#   scripts/e2e/remote-exec.sh [INSTANCE] [--deadline S] < script.sh
#
# INSTANCE defaults to FR_E2E_CONTROL_INSTANCE. The command runs as root under
# /bin/sh via AWS-RunShellScript:
#   - several arguments are quoted one by one, so they arrive exactly as given
#     (no re-parsing of '&', '$' or spaces: safe for presigned URLs);
#   - a single argument is run as a shell script, so pipes and && work;
#   - with no command, the script is read from stdin.
#
# The command is killed on the instance after --deadline seconds
# (FR_E2E_EXEC_DEADLINE, default 300), so one that never exits, such as a
# tcpdump without -c, cannot hang the caller. Bound such commands yourself
# (tcpdump -c N, timeout 30 ...) to keep their output.
#
# Prints the command's stdout on stdout and its stderr on stderr, and exits
# with its exit status. SSM keeps only the first 24000 characters of each
# stream: redirect larger output to a file on the instance and read it in parts.
# Exit 124 if the command timed out, 2 on bad usage or configuration.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

usage() {
	sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

instance="" deadline=""
while [ $# -gt 0 ]; do
	case "$1" in
	--) shift && break ;;
	--deadline) deadline="${2:-}" && shift ;;
	-h | --help) usage ;;
	-*) e2e_log "unknown option: $1" && usage 2 >&2 ;;
	*)
		[ -z "$instance" ] || e2e_die 2 "more than one instance given; put the command after --"
		instance="$1"
		;;
	esac
	shift
done
FR_E2E_EXEC_DEADLINE="${deadline:-${FR_E2E_EXEC_DEADLINE:-}}"
deadline="$(e2e_seconds FR_E2E_EXEC_DEADLINE 300)"
poll="$(e2e_seconds FR_E2E_POLL 5)"
instance="$(e2e_instance "$instance")"
e2e_aws
e2e_need_cmd jq

# sh_quote ARG: ARG as one POSIX-shell word.
sh_quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

if [ $# -eq 0 ]; then
	[ ! -t 0 ] || e2e_die 2 "no command: put it after --, or pipe a script on stdin"
	script="$(cat)"
elif [ $# -eq 1 ]; then
	script="$1"
else
	script=""
	for a in "$@"; do script="$script$(sh_quote "$a") "; done
fi
[ -n "$script" ] || e2e_die 2 "empty command"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
jq -n --arg c "$script" --arg t "$deadline" '{commands: [$c], executionTimeout: [$t]}' >"$work/params.json"

cid="$(aws ssm send-command --instance-ids "$instance" --document-name AWS-RunShellScript \
	--timeout-seconds 60 --parameters "file://$work/params.json" \
	--comment "firerunner e2e remote-exec" --query Command.CommandId --output text)" ||
	e2e_die 1 "send-command to $instance failed; is it running, with the SSM agent online and an instance role that allows SSM?"
e2e_log "command $cid on $instance (deadline ${deadline}s)"

# The instance enforces the deadline; this one, with slack for delivery and
# reporting, is in case it does not.
give_up=$(($(e2e_now) + deadline + 60))
status=""
while :; do
	if inv="$(aws ssm get-command-invocation --command-id "$cid" --instance-id "$instance" --output json 2>"$work/err")"; then
		status="$(jq -r '.Status // ""' <<<"$inv")"
		case "$status" in
		Pending | InProgress | Delayed | Cancelling | "") ;;
		*) break ;;
		esac
	fi
	# Right after send-command the invocation may not exist yet: keep polling.
	if [ "$(e2e_now)" -ge "$give_up" ]; then
		aws ssm cancel-command --command-id "$cid" --instance-ids "$instance" >/dev/null 2>&1 || true
		e2e_die 124 "no result for $cid after $((deadline + 60))s (last status '${status:-unknown}': $(cat "$work/err"))"
	fi
	sleep "$poll"
done

# Non-empty output ends with a newline, so the log lines after it stay apart.
show() { jq -j "$1"' // "" | if . == "" or endswith("\n") then . else . + "\n" end' <<<"$inv"; }
show .StandardOutputContent
show .StandardErrorContent >&2
code="$(jq -r '.ResponseCode // -1' <<<"$inv")"
case "$status" in
Success) exit "$code" ;;
TimedOut)
	e2e_log "timed out after ${deadline}s on $instance"
	exit 124
	;;
*)
	e2e_log "ended $status ($(jq -r '.StatusDetails // ""' <<<"$inv")), exit code $code"
	[ "$code" -gt 0 ] 2>/dev/null && exit "$code"
	exit 1
	;;
esac
