#!/usr/bin/env bash
# watch-run.sh — wait for a workflow run, failing as early as the run allows.
#
# Usage: scripts/e2e/watch-run.sh RUN_ID
#
# Unlike `gh run watch`, it stops as soon as the answer is known:
#   - exit 0 when the run completes with success;
#   - exit 1 as soon as any job fails (without waiting for the rest), or the
#     run completes with any other conclusion;
#   - exit 3 if no job has started FR_E2E_START_DEADLINE seconds (300) after
#     the run was created: no runner picked it up, so look at firerunner's log
#     and the runner instance's console rather than wait;
#   - exit 124 if the run is still going after FR_E2E_RUN_DEADLINE seconds (1800).
#
# Job transitions are logged on stderr. The last line on stdout is one JSON
# object: {"run":…,"status":…,"conclusion":…,"verdict":…,"url":…,"failed_jobs":[…]}.
# Needs FR_E2E_REPO.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

case "${1:-}" in
-h | --help | "")
	sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
	[ -n "${1:-}" ] && exit 0
	exit 2
	;;
esac
run_id="$1"
case "$run_id" in *[!0-9]*) e2e_die 2 "RUN_ID must be a number, got '$run_id'" ;; esac
[ $# -eq 1 ] || e2e_die 2 "one RUN_ID only"
start_deadline="$(e2e_seconds FR_E2E_START_DEADLINE 300)"
run_deadline="$(e2e_seconds FR_E2E_RUN_DEADLINE 1800)"
poll="$(e2e_seconds FR_E2E_POLL 5)"
e2e_gh
repo="$FR_E2E_REPO"

run='{}' jobs='{"jobs":[]}' seen=""
give_up=$(($(e2e_now) + run_deadline))
err="$(mktemp)"
trap 'rm -f "$err"' EXIT

# finish RC VERDICT: print the JSON summary and exit.
finish() {
	jq -cn --argjson run "$run" --argjson jobs "$jobs" --arg id "$run_id" --arg verdict "$2" '{
		run: ($id | tonumber), status: ($run.status // null), conclusion: ($run.conclusion // null),
		verdict: $verdict, url: ($run.html_url // null),
		failed_jobs: [$jobs.jobs[] | select(.conclusion | IN("failure", "cancelled", "timed_out", "startup_failure")) | {name, conclusion, url: .html_url}]
	}'
	exit "$1"
}

while :; do
	if r="$(gh api "repos/$repo/actions/runs/$run_id" 2>"$err")" &&
		j="$(gh api "repos/$repo/actions/runs/$run_id/jobs?per_page=100" 2>"$err")"; then
		run="$r" jobs="$j"
	else
		grep -q "Not Found" "$err" && e2e_die 2 "run $run_id not found in $repo"
		# Any other API error is retried until the run deadline.
		e2e_log "gh api: $(cat "$err")"
	fi

	# Log each job's transitions once.
	while IFS= read -r line; do
		case "$seen" in *"|$line|"*) continue ;; esac
		seen="$seen|$line|"
		e2e_log "job $line"
	done < <(jq -r '.jobs[] | "\(.name): \(.status)\(if .conclusion then " (\(.conclusion))" else "" end)"' <<<"$jobs")

	status="$(jq -r '.status // ""' <<<"$run")"
	if [ "$(jq '[.jobs[] | select(.conclusion | IN("failure", "cancelled", "timed_out", "startup_failure"))] | length' <<<"$jobs")" -gt 0 ]; then
		e2e_log "a job failed; its log: gh run view $run_id --repo $repo --log-failed"
		finish 1 job_failed
	fi
	if [ "$status" = completed ]; then
		[ "$(jq -r '.conclusion // ""' <<<"$run")" = success ] && finish 0 success
		finish 1 "run_$(jq -r '.conclusion // "unknown"' <<<"$run")"
	fi

	now="$(e2e_now)"
	created="$(jq -r '(.created_at // empty) | fromdateiso8601' <<<"$run" 2>/dev/null || true)"
	started="$(jq '[.jobs[] | select(.status == "in_progress" or .status == "completed")] | length' <<<"$jobs")"
	if [ -n "$created" ] && [ "$started" = 0 ] && [ $((now - created)) -ge "$start_deadline" ]; then
		e2e_log "no job of run $run_id started within ${start_deadline}s of its creation: no runner picked it up."
		e2e_log "check firerunner's log:  scripts/e2e/remote-exec.sh -- journalctl -u firerunner -n 200 --no-pager"
		e2e_log "and a runner's console:  scripts/e2e/console.sh <runner-instance-id>"
		e2e_log "(runner instances carry the firerunner:tier tag)"
		finish 3 no_job_started
	fi
	if [ "$now" -ge "$give_up" ]; then
		e2e_log "run $run_id still $status after ${run_deadline}s"
		finish 124 deadline
	fi
	sleep "$poll"
done
