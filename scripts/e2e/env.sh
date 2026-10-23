#!/usr/bin/env bash
# env.sh — configuration and helpers shared by the scripts/e2e harness.
#
# Sourced by every e2e script. Run directly, it prints the configuration and
# exits non-zero if anything required is missing:
#
#   scripts/e2e/env.sh
#
# Configuration comes only from the environment; nothing here names an
# account, a host or a repository. Export the variables once per shell, so
# every aws and gh call made by the harness picks them up:
#
#   AWS_REGION              region of the control VM and runners (aws commands)
#   AWS_PROFILE             optional: CLI profile; otherwise the default chain
#   FR_E2E_REPO             OWNER/REPO the e2e workflows run in (gh commands)
#   FR_E2E_WORKFLOW         optional: default workflow file for dispatch-and-measure
#   FR_E2E_REF              optional: branch or tag to dispatch on (default: the
#                           repository's default branch)
#   FR_E2E_CONTROL_INSTANCE optional: control VM instance ID, the default target
#                           of remote-exec and console
#   FR_E2E_START_DEADLINE   seconds to wait for a dispatched job to start (300)
#   FR_E2E_RUN_DEADLINE     seconds to wait for a run to complete (1800)
#   FR_E2E_EXEC_DEADLINE    seconds a remote-exec command may run (300)
#   FR_E2E_POLL             seconds between polls (5)
#
# Exit statuses shared by the harness:
#   0 success, 1 the thing under test failed, 2 usage or configuration error,
#   3 no job started within FR_E2E_START_DEADLINE, 124 a deadline passed.

# Never page: a pager waits for a terminal an agent does not have.
export AWS_PAGER="" GH_PAGER=cat PAGER=cat

e2e_name="${e2e_name:-$(basename "$0" .sh)}"

e2e_log() { echo "$e2e_name: $*" >&2; }
e2e_die() {
	local rc="$1"
	shift
	e2e_log "$*"
	exit "$rc"
}

# e2e_require VAR...: fail with exit 2, naming every unset variable at once.
e2e_require() {
	local v missing=""
	for v in "$@"; do
		[ -n "${!v:-}" ] || missing="$missing $v"
	done
	[ -z "$missing" ] && return 0
	e2e_log "missing configuration:$missing"
	e2e_log "export them first (see scripts/e2e/README.md), for example:"
	for v in $missing; do
		e2e_log "  export $v=..."
	done
	exit 2
}

# e2e_need_cmd CMD...: fail with exit 2 if a command is not on PATH.
e2e_need_cmd() {
	local c
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 || e2e_die 2 "'$c' is not on PATH (PATH=$PATH)"
	done
}

# e2e_seconds VAR DEFAULT: print VAR's value (or DEFAULT), checked to be a
# whole number of seconds.
e2e_seconds() {
	local val="${!1:-$2}"
	case "$val" in
	'' | *[!0-9]*) e2e_die 2 "$1 must be a whole number of seconds, got '$val'" ;;
	esac
	echo "$val"
}

e2e_now() { date +%s; }

e2e_aws() {
	e2e_require AWS_REGION
	e2e_need_cmd aws
}
e2e_gh() {
	e2e_require FR_E2E_REPO
	e2e_need_cmd gh jq
	case "$FR_E2E_REPO" in
	?*/?*) ;;
	*) e2e_die 2 "FR_E2E_REPO must be OWNER/REPO, got '$FR_E2E_REPO'" ;;
	esac
}

# e2e_instance ARG: the instance an aws command targets: ARG, else
# FR_E2E_CONTROL_INSTANCE.
e2e_instance() {
	local id="${1:-${FR_E2E_CONTROL_INSTANCE:-}}"
	[ -n "$id" ] || e2e_die 2 "no instance: pass one or export FR_E2E_CONTROL_INSTANCE"
	case "$id" in
	i-*) echo "$id" ;;
	*) e2e_die 2 "'$id' is not an instance ID (i-...)" ;;
	esac
}

# Run directly: print the configuration.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -u
	rc=0
	for v in AWS_REGION FR_E2E_REPO; do
		if [ -n "${!v:-}" ]; then echo "set      $v=${!v}"; else echo "MISSING  $v" && rc=2; fi
	done
	for v in AWS_PROFILE FR_E2E_WORKFLOW FR_E2E_REF FR_E2E_CONTROL_INSTANCE \
		FR_E2E_START_DEADLINE FR_E2E_RUN_DEADLINE FR_E2E_EXEC_DEADLINE FR_E2E_POLL; do
		if [ -n "${!v:-}" ]; then echo "set      $v=${!v}"; else echo "default  $v"; fi
	done
	for c in aws gh jq; do
		if command -v "$c" >/dev/null 2>&1; then echo "found    $c"; else echo "MISSING  $c (not on PATH)" && rc=2; fi
	done
	exit "$rc"
fi
