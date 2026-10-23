#!/usr/bin/env bash
# dispatch-and-measure.sh — dispatch a workflow N times and time its jobs.
#
# Usage: scripts/e2e/dispatch-and-measure.sh [WORKFLOW] N [-f KEY=VALUE]...
#
# WORKFLOW is a workflow file name in FR_E2E_REPO (default FR_E2E_WORKFLOW),
# dispatched N times on FR_E2E_REF (default: the default branch). -f passes
# workflow_dispatch inputs, as with `gh workflow run`.
#
# As each run completes it prints one JSON line per job on stdout:
#   {"run":…,"job":"…","conclusion":"…","queue_s":…,"run_s":…,"total_s":…}
# queue_s is the job's created -> started time (the launch latency a runner
# tier adds), run_s its started -> completed time. A last line summarises:
#   {"summary":true,"runs":N,"jobs":…,"failed":…,"failed_runs":…,"queue_s":{"p50":…,"max":…},"run_s":{…}}
#
# Exit 0 if every run and job succeeded, 1 if any failed, 3 if the runs did not appear
# or no job started within FR_E2E_START_DEADLINE seconds (300) of dispatch,
# 124 if runs were still going after FR_E2E_RUN_DEADLINE seconds (1800).
# Runs are matched by workflow, event and creation time: avoid dispatching the
# same workflow by other means while this measures.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

usage() {
	sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

pos=() inputs=()
while [ $# -gt 0 ]; do
	case "$1" in
	-f)
		case "${2:-}" in ?*=*) ;; *) e2e_die 2 "-f wants KEY=VALUE, got '${2:-}'" ;; esac
		inputs+=(-f "$2") && shift
		;;
	-h | --help) usage ;;
	-*) e2e_log "unknown option: $1" && usage 2 >&2 ;;
	*) pos+=("$1") ;;
	esac
	shift
done
case "${#pos[@]}" in
1) workflow="${FR_E2E_WORKFLOW:-}" n="${pos[0]}" ;;
2) workflow="${pos[0]}" n="${pos[1]}" ;;
*) e2e_log "want [WORKFLOW] N" && usage 2 >&2 ;;
esac
[ -n "$workflow" ] || e2e_die 2 "no workflow: pass one or export FR_E2E_WORKFLOW"
case "$n" in '' | *[!0-9]* | 0) e2e_die 2 "N must be a positive number, got '$n'" ;; esac
start_deadline="$(e2e_seconds FR_E2E_START_DEADLINE 300)"
run_deadline="$(e2e_seconds FR_E2E_RUN_DEADLINE 1800)"
poll="$(e2e_seconds FR_E2E_POLL 5)"
e2e_gh
repo="$FR_E2E_REPO"
ref_args=()
[ -z "${FR_E2E_REF:-}" ] || ref_args=(--ref "$FR_E2E_REF")

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
: >"$work/jobs.jsonl"
: >"$work/runs.txt"

summary() {
	local failed_runs
	failed_runs="$(grep -cv '^success$' "$work/runs.txt" || true)"
	jq -cs --argjson runs "$n" --argjson failed_runs "$failed_runs" '
		def stat(f): [.[] | f | numbers] | sort | if length == 0 then {p50: null, max: null}
			else {p50: .[(length - 1) / 2 | floor], max: .[-1]} end;
		{summary: true, runs: $runs, jobs: length,
		 failed: [.[] | select(.conclusion != "success")] | length, failed_runs: $failed_runs,
		 queue_s: stat(.queue_s), run_s: stat(.run_s)}' "$work/jobs.jsonl"
}

# GitHub's clock and ours may differ by a few seconds.
t0="$(e2e_now)"
since=$((t0 - 10))
i=0
while [ "$i" -lt "$n" ]; do
	gh workflow run "$workflow" --repo "$repo" "${ref_args[@]+"${ref_args[@]}"}" "${inputs[@]+"${inputs[@]}"}" >/dev/null ||
		e2e_die 1 "dispatching $workflow failed (run $((i + 1)) of $n)"
	i=$((i + 1))
done
e2e_log "dispatched $workflow x$n in $repo; waiting for the runs"

# The dispatched runs: the newest N of this workflow's dispatch runs since t0.
runs=""
while :; do
	runs="$(gh api "repos/$repo/actions/workflows/$workflow/runs?event=workflow_dispatch&per_page=100" 2>"$work/err" |
		jq -r --argjson since "$since" --argjson n "$n" \
			'[.workflow_runs[] | select((.created_at | fromdateiso8601) >= $since)] | sort_by(.created_at) | .[-$n:][] | .id' |
		tr '\n' ' ')" || runs=""
	# shellcheck disable=SC2086 # count the words
	set -- $runs
	[ "$#" -ge "$n" ] && break
	if [ $(($(e2e_now) - t0)) -ge "$start_deadline" ]; then
		e2e_die 3 "only $# of $n runs appeared within ${start_deadline}s $(cat "$work/err")"
	fi
	sleep "$poll"
done
e2e_log "runs: $runs"

pending="$runs" started=0
while :; do
	still=""
	for id in $pending; do
		if ! run="$(gh api "repos/$repo/actions/runs/$id" 2>"$work/err")" ||
			! jobs="$(gh api "repos/$repo/actions/runs/$id/jobs?per_page=100" 2>"$work/err")"; then
			e2e_log "gh api (retrying): $(cat "$work/err")"
			still="$still $id"
			continue
		fi
		if [ "$started" = 0 ] && [ "$(jq '[.jobs[] | select(.status == "in_progress" or .status == "completed")] | length' <<<"$jobs")" -gt 0 ]; then
			started=1
			e2e_log "first job started after $(($(e2e_now) - t0))s"
		fi
		if [ "$(jq -r .status <<<"$run")" != completed ]; then
			still="$still $id"
			continue
		fi
		jq -r '.conclusion // "unknown"' <<<"$run" >>"$work/runs.txt"
		# A job that never started (startup_failure) has no times: null.
		jq -c --argjson run "$id" 'def t: if . then fromdateiso8601 else null end;
			def d(a; b): if a and b then b - a else null end;
			.jobs[] | select(.conclusion != "skipped") |
			(.created_at | t) as $c | (.started_at | t) as $s | (.completed_at | t) as $e |
			{run: $run, job: .name, conclusion, queue_s: d($c; $s), run_s: d($s; $e), total_s: d($c; $e)}' <<<"$jobs" |
			tee -a "$work/jobs.jsonl"
	done
	pending="$still"
	[ -n "$pending" ] || break

	elapsed=$(($(e2e_now) - t0))
	if [ "$started" = 0 ] && [ "$elapsed" -ge "$start_deadline" ]; then
		e2e_log "no job started within ${start_deadline}s of dispatch: no runner picked them up."
		e2e_log "check firerunner's log:  scripts/e2e/remote-exec.sh -- journalctl -u firerunner -n 200 --no-pager"
		e2e_log "and a runner's console:  scripts/e2e/console.sh <runner-instance-id>"
		e2e_log "(runner instances carry the firerunner:tier tag)"
		summary
		exit 3
	fi
	if [ "$elapsed" -ge "$run_deadline" ]; then
		e2e_log "runs still going after ${run_deadline}s:$pending"
		summary
		exit 124
	fi
	sleep "$poll"
done

summary
[ "$(summary | jq '.failed + .failed_runs')" = 0 ]
