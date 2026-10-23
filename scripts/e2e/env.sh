#!/usr/bin/env bash
# env.sh — configuration and helpers shared by the scripts/e2e harness.
#
# Sourced by every e2e script. Run directly, it prints the configuration and
# the live control VMs with their owners, and exits non-zero if anything
# required is missing:
#
#   scripts/e2e/env.sh             # what is set, and who else is using the environment
#   scripts/e2e/env.sh --exports   # export lines for the values read from .claude/e2e.env
#
# Configuration comes from the environment and from .claude/e2e.env (below);
# nothing in the repository names an account, a host or a repository:
#
#   AWS_REGION              region of the control VM and runners (aws commands)
#   AWS_PROFILE             optional: CLI profile; otherwise the default chain
#   FR_E2E_REPO             OWNER/REPO the e2e workflows run in (gh commands)
#   FR_OWNER                optional: this session's firerunner:owner tag
#                           (default: e2e-<worktree or branch name>)
#   FR_E2E_TIER_SUFFIX      optional: appended to tier names, which are scale
#                           set names (default: FR_OWNER without "e2e-")
#   FR_E2E_STACK            control-vm: the shared stack made from
#                           deploy/aws/control.yaml that session stacks copy
#   FR_E2E_BUCKET           control-vm install: S3 bucket for a dev binary
#   FR_E2E_VERSION          control-vm up: release to run (default: the shared
#                           stack's version)
#   FR_E2E_ARTIFACTS        where watch-run saves consoles (default: a
#                           directory under $TMPDIR)
#   FR_E2E_WORKFLOW         optional: default workflow file for dispatch-and-measure
#   FR_E2E_REF              optional: branch or tag to dispatch on (default: the
#                           repository's default branch)
#   FR_E2E_CONTROL_INSTANCE optional: control VM instance ID, the default target
#                           of remote-exec and console
#   FR_E2E_START_DEADLINE   seconds to wait for a dispatched job to start (300)
#   FR_E2E_RUN_DEADLINE     seconds to wait for a run to complete (1800)
#   FR_E2E_EXEC_DEADLINE    seconds a remote-exec command may run (300)
#   FR_E2E_STACK_DEADLINE   seconds for a control VM to come up or go (1200)
#   FR_E2E_POLL             seconds between polls (5)
#
# .claude/e2e.env (gitignored) holds KEY=VALUE lines for AWS_PROFILE,
# AWS_REGION and FR_* variables. Values are literal: no expansion. A
# variable already in the environment wins. Read, in order: $FR_E2E_ENV_FILE
# ("none" reads no file at all), this checkout's .claude/e2e.env, then the
# main checkout's when this is a worktree. FR_OWNER and FR_E2E_TIER_SUFFIX
# are per session, so the main checkout's file may not set them for a
# worktree.
#
# Exit statuses shared by the harness:
#   0 success, 1 the thing under test failed, 2 usage or configuration error,
#   3 no job started within FR_E2E_START_DEADLINE, 124 a deadline passed.

# Never page: a pager waits for a terminal an agent does not have.
export AWS_PAGER="" GH_PAGER=cat PAGER=cat

if [ -z "${e2e_name:-}" ]; then
	e2e_name="${0##*/}"
	e2e_name="${e2e_name%.sh}"
fi
e2e_here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

e2e_log() { echo "$e2e_name: $*" >&2; }
e2e_die() {
	local rc="$1"
	shift
	e2e_log "$*"
	exit "$rc"
}

# --- configuration files -----------------------------------------------------

e2e_owner_keys="FR_OWNER FR_E2E_TIER_SUFFIX"
e2e_files_read="" # the files read, for the report
e2e_from_files="" # the variables they set

# The checkout holding these scripts, and its main checkout if it is a worktree.
e2e_checkout() { git -C "$e2e_here" rev-parse --show-toplevel 2>/dev/null; }
e2e_main_checkout() {
	local c
	c="$(git -C "$e2e_here" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || return 1
	case "$c" in */.git) dirname "$c" ;; *) return 1 ;; esac
}

# e2e_read_env FILE SHARED: export each KEY=VALUE line of FILE whose KEY is
# not set yet. SHARED=1 refuses the per-session keys.
e2e_read_env() {
	local file="$1" shared="$2" line key val n=0
	[ -f "$file" ] || return 0
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		line="${line#"${line%%[![:space:]]*}"}"
		case "$line" in '' | '#'*) continue ;; esac
		line="${line#export }"
		case "$line" in
		*=*) key="${line%%=*}" val="${line#*=}" ;;
		*)
			e2e_log "$file:$n: not KEY=VALUE, ignored"
			continue
			;;
		esac
		case "$key" in
		AWS_PROFILE | AWS_REGION | AWS_DEFAULT_REGION) ;;
		FR_*) case "$key" in *[!A-Z0-9_]*)
			e2e_log "$file:$n: '$key' is not a variable name, ignored"
			continue
			;;
		esac ;;
		*)
			e2e_log "$file:$n: $key is not AWS_PROFILE, AWS_REGION or FR_*, ignored"
			continue
			;;
		esac
		case "$val" in
		\"*\") val="${val#\"}" val="${val%\"}" ;;
		\'*\') val="${val#\'}" val="${val%\'}" ;;
		esac
		if [ "$shared" = 1 ]; then
			case " $e2e_owner_keys " in *" $key "*)
				e2e_log "$file:$n: $key is per session and ignored here; set it in this worktree's .claude/e2e.env"
				continue
				;;
			esac
		fi
		[ -n "${!key+x}" ] && continue
		export "$key=$val"
		e2e_from_files="$e2e_from_files $key"
	done <"$file"
	e2e_files_read="$e2e_files_read $file"
}

e2e_load_config() {
	local own main
	case "${FR_E2E_ENV_FILE:-}" in
	none) return 0 ;;
	'') ;;
	*)
		[ -f "$FR_E2E_ENV_FILE" ] || e2e_die 2 "FR_E2E_ENV_FILE=$FR_E2E_ENV_FILE does not exist"
		e2e_read_env "$FR_E2E_ENV_FILE" 0
		;;
	esac
	own="$(e2e_checkout)" || return 0
	e2e_read_env "$own/.claude/e2e.env" 0
	main="$(e2e_main_checkout)" || return 0
	[ "$main" = "$own" ] || e2e_read_env "$main/.claude/e2e.env" 1
}
e2e_load_config

# --- helpers -----------------------------------------------------------------

# e2e_require VAR...: fail with exit 2, naming every unset variable at once.
e2e_require() {
	local v missing=""
	for v in "$@"; do
		[ -n "${!v:-}" ] || missing="$missing $v"
	done
	[ -z "$missing" ] && return 0
	e2e_log "missing configuration:$missing"
	e2e_log "export them, or put them in .claude/e2e.env (see scripts/e2e/README.md), for example:"
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

# --- owner and tier names ----------------------------------------------------

# e2e_slug TEXT: lower-case letters, digits and single hyphens, at most 24.
e2e_slug() {
	printf '%s' "$1" | LC_ALL=C tr '[:upper:]' '[:lower:]' | LC_ALL=C tr -c 'a-z0-9' '-' | tr -s '-' | sed 's/^-*//' | cut -c1-24 | sed 's/-*$//'
}

# e2e_check_name WHAT VALUE: a name that fits a stack, a DNS label and a tag.
e2e_check_name() {
	case "$2" in
	'' | -* | *[!A-Za-z0-9-]*) e2e_die 2 "$1 must be letters, digits and hyphens, not starting with one; got '$2'" ;;
	esac
	[ "${#2}" -le 40 ] || e2e_die 2 "$1 must be at most 40 characters, got '$2'"
}

# e2e_owner: this session's owner tag value. FR_OWNER, else e2e-<name> from
# the worktree's directory name or, outside a worktree, the branch.
e2e_owner() {
	local top src=""
	if [ -n "${FR_OWNER:-}" ]; then
		e2e_check_name FR_OWNER "$FR_OWNER"
		echo "$FR_OWNER"
		return 0
	fi
	top="$(e2e_checkout)" || e2e_die 2 "not in a git checkout, so no owner to derive: export FR_OWNER=e2e-<name>"
	case "$top" in */.claude/worktrees/*) src="$(basename "$top")" ;; esac
	[ -n "$src" ] || src="$(git -C "$top" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
	case "$src" in
	'' | HEAD | main | master)
		e2e_die 2 "no session owner to derive from branch '${src:-?}', which other sessions share: export FR_OWNER=e2e-<name> (see scripts/e2e/README.md)"
		;;
	esac
	src="$(e2e_slug "$src")"
	[ -n "$src" ] || e2e_die 2 "no owner to derive: export FR_OWNER=e2e-<name>"
	echo "e2e-$src"
}

# e2e_tier_suffix: what this session appends to tier names.
e2e_tier_suffix() {
	local s="${FR_E2E_TIER_SUFFIX:-}" o
	if [ -z "$s" ]; then
		o="$(e2e_owner)"
		s="${o#e2e-}"
	fi
	e2e_check_name FR_E2E_TIER_SUFFIX "$s"
	echo "$s"
}

# --- who is using the environment --------------------------------------------

# e2e_control_vms: a JSON array of the control VMs not terminated, oldest
# first: {instance, state, name, stack, owner, launched}. A VM made by
# control-vm.sh carries its owner in the firerunner:e2e-session tag; for
# another stack's VM the owner is the stack's Owner parameter; for a VM made
# by hand it is null.
e2e_control_vms() {
	local out s o owners='{}'
	out="$(aws ec2 describe-instances --output json \
		--filters 'Name=tag:Name,Values=*control*' Name=instance-state-name,Values=pending,running,stopping,stopped)" || return 1
	while IFS= read -r s; do
		[ -n "$s" ] || continue
		o="$(aws cloudformation describe-stacks --stack-name "$s" \
			--query "Stacks[0].Parameters[?ParameterKey=='Owner'].ParameterValue" --output text 2>/dev/null)" || o=""
		case "$o" in '' | None) continue ;; esac
		owners="$(jq -c --arg s "$s" --arg o "$o" '. + {($s): $o}' <<<"$owners")"
	done < <(jq -r '[.Reservations[].Instances[] | (.Tags // []) | from_entries
		| select(has("firerunner:e2e-session") | not) | .["firerunner:control"] // empty] | unique[]' <<<"$out")
	jq -c --argjson owners "$owners" '[.Reservations[].Instances[] | ((.Tags // []) | from_entries) as $t | {
		instance: .InstanceId, state: .State.Name, name: ($t.Name // null),
		stack: ($t["firerunner:control"] // null),
		owner: ($t["firerunner:e2e-session"] // $owners[$t["firerunner:control"] // ""] // null),
		launched: (.LaunchTime // null)
	}] | sort_by(.launched)' <<<"$out"
}

# Run directly: print the configuration.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -u
	case "${1:-}" in
	--exports)
		for k in $e2e_from_files; do
			case " $e2e_owner_keys " in *" $k "*) continue ;; esac
			printf "export %s='%s'\n" "$k" "$(printf '%s' "${!k}" | sed "s/'/'\\\\''/g")"
		done
		exit 0
		;;
	-h | --help)
		sed -n '2,49p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	'') ;;
	*) e2e_die 2 "unknown argument: $1 (see --help)" ;;
	esac
	rc=0
	for f in $e2e_files_read; do echo "read     $f"; done
	for v in AWS_REGION FR_E2E_REPO; do
		if [ -n "${!v:-}" ]; then echo "set      $v=${!v}"; else echo "MISSING  $v" && rc=2; fi
	done
	if owner="$(e2e_owner 2>/dev/null)"; then
		echo "owner    FR_OWNER=$owner${FR_OWNER:+ (set)}"
		echo "owner    FR_E2E_TIER_SUFFIX=$(e2e_tier_suffix 2>/dev/null || echo '?')"
	else
		echo "MISSING  FR_OWNER (cannot derive one here; export FR_OWNER=e2e-<name>)" && rc=2
	fi
	for v in AWS_PROFILE FR_E2E_STACK FR_E2E_BUCKET FR_E2E_VERSION FR_E2E_ARTIFACTS \
		FR_E2E_WORKFLOW FR_E2E_REF FR_E2E_CONTROL_INSTANCE FR_E2E_START_DEADLINE \
		FR_E2E_RUN_DEADLINE FR_E2E_EXEC_DEADLINE FR_E2E_STACK_DEADLINE FR_E2E_POLL; do
		if [ -n "${!v:-}" ]; then echo "set      $v=${!v}"; else echo "default  $v"; fi
	done
	for c in aws gh jq; do
		if command -v "$c" >/dev/null 2>&1; then echo "found    $c"; else echo "MISSING  $c (not on PATH)" && rc=2; fi
	done
	if [ -n "${AWS_REGION:-}" ] && command -v aws >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
		if vms="$(e2e_control_vms 2>/dev/null)"; then
			echo "control VMs (owner instance state launched), the sessions using this environment:"
			jq -r 'if length == 0 then "  none" else .[] | "  \(.owner // "unknown") \(.instance) \(.state) \(.launched // "")" end' <<<"$vms"
		else
			echo "control VMs: could not list them (aws ec2 describe-instances failed)"
		fi
	fi
	exit "$rc"
fi
