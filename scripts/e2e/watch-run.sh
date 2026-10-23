#!/usr/bin/env bash
# watch-run.sh — wait for a workflow run, failing as early as the run allows.
#
# Usage: scripts/e2e/watch-run.sh RUN_ID
#
# Unlike `gh run watch`, it stops as soon as the answer is known:
#   - exit 0 when the run completes with success;
#   - exit 1 as soon as any job fails (without waiting for the rest), or the
#     run completes with any other conclusion. The console of each failed
#     job's runner instance is saved first, before the instance is gone;
#   - exit 3 if no job has started FR_E2E_START_DEADLINE seconds (300) after
#     the run was created: no runner picked it up, so look at firerunner's log
#     and the runner instance's console rather than wait. The consoles of
#     this session's runner instances (firerunner:owner=FR_OWNER) launched
#     since the run was created are saved;
#   - exit 124 if the run is still going after FR_E2E_RUN_DEADLINE seconds (1800).
#
# Job transitions are logged on stderr. The last line on stdout is one JSON
# object: {"run":…,"status":…,"conclusion":…,"verdict":…,"url":…,"failed_jobs":[…],
# "consoles":[{"instance":…,"runner":…,"file":…}]}. Consoles go to
# FR_E2E_ARTIFACTS (default $TMPDIR/firerunner-e2e/<owner>), one file per
# instance, run-<RUN_ID>-<instance>.console; they need AWS_REGION and aws,
# and are skipped without them. Needs FR_E2E_REPO.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

case "${1:-}" in
-h | --help | "")
	sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'
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
# Only to find this session's instances; watching works without one.
owner="$(e2e_owner 2>/dev/null || true)"

run='{}' jobs='{"jobs":[]}' seen="" consoles='[]'
give_up=$(($(e2e_now) + run_deadline))
err="$(mktemp)"
trap 'rm -f "$err"' EXIT

# finish RC VERDICT: print the JSON summary and exit.
finish() {
	jq -cn --argjson run "$run" --argjson jobs "$jobs" --arg id "$run_id" --arg verdict "$2" --argjson consoles "$consoles" '{
		run: ($id | tonumber), status: ($run.status // null), conclusion: ($run.conclusion // null),
		verdict: $verdict, url: ($run.html_url // null),
		failed_jobs: [$jobs.jobs[] | select(.conclusion | IN("failure", "cancelled", "timed_out", "startup_failure")) | {name, conclusion, runner: (.runner_name // null), url: .html_url}],
		consoles: $consoles
	}'
	exit "$1"
}

# save_consoles FILTER...: save the console of every instance that
# describe-instances finds with these filters, in any state (a terminated
# instance's last console stays readable for a while), and add each to
# $consoles. With $since set, only instances launched at or after it.
save_consoles() {
	local out dir id runner file
	if [ -z "${AWS_REGION:-}" ] || ! command -v aws >/dev/null 2>&1; then
		e2e_log "not saving consoles: AWS_REGION is not set or aws is not on PATH"
		return 0
	fi
	out="$(aws ec2 describe-instances --output json --filters "$@" 2>"$err")" || {
		e2e_log "not saving consoles: describe-instances: $(cat "$err")"
		return 0
	}
	dir="${FR_E2E_ARTIFACTS:-${TMPDIR:-/tmp}/firerunner-e2e/${owner:-unknown}}"
	mkdir -p "$dir"
	while IFS=' ' read -r id runner; do
		[ -n "$id" ] || continue
		file="$dir/run-$run_id-$id.console"
		if "$HERE/console.sh" "$id" -n 0 >"$file" 2>"$err"; then
			e2e_log "saved the console of $id (runner ${runner:-none}) to $file"
			consoles="$(jq -c --arg i "$id" --arg r "$runner" --arg f "$file" '. + [{instance: $i, runner: (if $r == "" then null else $r end), file: $f}]' <<<"$consoles")"
		else
			e2e_log "no console for $id: $(tail -n 1 "$err")"
			rm -f "$file"
		fi
	done < <(jq -r --argjson since "${since:-0}" '.Reservations[].Instances[]
		| select(((.LaunchTime // "") | sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | (try fromdateiso8601 catch 0)) >= $since)
		| "\(.InstanceId) \(((.Tags // []) | from_entries)["firerunner:runner"] // "")"' <<<"$out")
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
		runners="$(jq -r '[.jobs[] | select(.conclusion | IN("failure", "cancelled", "timed_out", "startup_failure")) | .runner_name // empty | select(. != "")] | unique | join(",")' <<<"$jobs")"
		if [ -n "$runners" ]; then
			save_consoles "Name=tag:firerunner:runner,Values=$runners"
		else
			e2e_log "the failed jobs name no runner, so there is no console to save"
		fi
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
		if [ -n "$owner" ]; then
			since="$created" save_consoles "Name=tag:firerunner:owner,Values=$owner"
		else
			e2e_log "not saving consoles: no FR_OWNER to find this session's instances by"
		fi
		finish 3 no_job_started
	fi
	if [ "$now" -ge "$give_up" ]; then
		e2e_log "run $run_id still $status after ${run_deadline}s"
		finish 124 deadline
	fi
	sleep "$poll"
done
