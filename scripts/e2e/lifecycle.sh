#!/usr/bin/env bash
# lifecycle.sh — time each job's runner instance, from launch to terminated.
#
# Usage: scripts/e2e/lifecycle.sh [WORKFLOW] N [-f KEY=VALUE]... [-i SECONDS]
#        scripts/e2e/lifecycle.sh --run RUN_ID [--run RUN_ID]... [-i SECONDS]
#
# The first form dispatches WORKFLOW (default FR_E2E_WORKFLOW) N times on
# FR_E2E_REF, as dispatch-and-measure.sh does; the second watches runs that
# are already queued. Start it before the jobs end: states are only seen
# while it samples.
#
# Every -i SECONDS (FR_E2E_INSTANCE_POLL, default 1) it samples the state of
# every instance carrying the firerunner:runner tag (describe-instances);
# GitHub is polled every FR_E2E_POLL seconds (5). A job is matched to the
# instance whose firerunner:runner tag is the job's runner_name. It stops once
# every run has completed and every started job's instance has been seen
# stopped or terminated.
#
# One JSON line per job on stdout:
#   {"run":…,"job":"…","job_id":…,"conclusion":"…","runner":"…","instance":"i-…",
#    "interval_s":1,"sample_gap_max_s":…,
#    "t":{"launch":…,"pending":…,"running":…,"job_start":…,"job_end":…,
#         "shutting_down":…,"stopping":…,"stopped":…,"terminated":…},
#    "err_s":{"running":…,…},"d":{"job_end_to_shutting_down_s":…,…}}
# t holds epoch seconds: launch is EC2's LaunchTime, job_start/job_end come
# from the GitHub API, and each state is the first sample that saw it. The
# transition happened up to err_s seconds before (the gap since the previous
# sample; null when the first sample already saw it), so a duration from a
# job time to a state is an upper bound, err_s at most above the truth, plus
# clock skew between GitHub, EC2 and this machine. null: not seen.
# d: launch_to_running_s, running_to_job_start_s, job_s,
# job_end_to_shutting_down_s, job_end_to_stopping_s, job_end_to_stopped_s,
# job_end_to_terminated_s, shutting_down_to_terminated_s.
# A last line summarises each duration as {"n","min","p50","max"}, with
# interval_s and sample_gap_max_s; fewer than 5 measured jobs sets
# "few_samples":true and warns on stderr.
#
# Exit 0 if every job succeeded and its instance was seen to end, 1 if a job
# failed or no instance matched it, 2 usage, configuration or a first
# describe-instances that fails, 3 no job started within
# FR_E2E_START_DEADLINE (300), 124 runs still going after FR_E2E_RUN_DEADLINE
# (1800), or instances not ended FR_E2E_SHUTDOWN_DEADLINE (600) after the
# last run completed. The report is printed in every case but 2.
# Needs AWS_REGION, FR_E2E_REPO, and ec2:DescribeInstances.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/e2e/env.sh
. "$HERE/env.sh"

usage() {
	awk 'NR == 1 { next } !/^#/ { exit } { sub(/^# ?/, ""); print }' "$0"
	exit "${1:-0}"
}

pos=() inputs=() given=""
interval="${FR_E2E_INSTANCE_POLL:-1}"
while [ $# -gt 0 ]; do
	case "$1" in
	-f)
		case "${2:-}" in ?*=*) ;; *) e2e_die 2 "-f wants KEY=VALUE, got '${2:-}'" ;; esac
		inputs+=(-f "$2") && shift
		;;
	-i)
		[ $# -ge 2 ] || e2e_die 2 "-i wants SECONDS"
		interval="$2" && shift
		;;
	--run)
		case "${2:-}" in '' | *[!0-9]*) e2e_die 2 "--run wants a RUN_ID, got '${2:-}'" ;; esac
		given="$given $2" && shift
		;;
	-h | --help) usage ;;
	-*) e2e_log "unknown option: $1" && usage 2 >&2 ;;
	*) pos+=("$1") ;;
	esac
	shift
done
case "$interval" in '' | *[!0-9]* | 0) e2e_die 2 "-i / FR_E2E_INSTANCE_POLL must be a positive whole number of seconds, got '$interval'" ;; esac
workflow="" n=0
if [ -n "$given" ]; then
	[ "${#pos[@]}" = 0 ] && [ "${#inputs[@]}" = 0 ] || e2e_die 2 "--run takes no WORKFLOW, N or -f"
	# shellcheck disable=SC2086 # count the words
	set -- $given
	n=$#
else
	case "${#pos[@]}" in
	1) workflow="${FR_E2E_WORKFLOW:-}" n="${pos[0]}" ;;
	2) workflow="${pos[0]}" n="${pos[1]}" ;;
	*) e2e_log "want [WORKFLOW] N, or --run RUN_ID" && usage 2 >&2 ;;
	esac
	[ -n "$workflow" ] || e2e_die 2 "no workflow: pass one or export FR_E2E_WORKFLOW"
	case "$n" in '' | *[!0-9]* | 0) e2e_die 2 "N must be a positive number, got '$n'" ;; esac
fi
start_deadline="$(e2e_seconds FR_E2E_START_DEADLINE 300)"
run_deadline="$(e2e_seconds FR_E2E_RUN_DEADLINE 1800)"
shutdown_deadline="$(e2e_seconds FR_E2E_SHUTDOWN_DEADLINE 600)"
poll="$(e2e_seconds FR_E2E_POLL 5)"
e2e_gh
e2e_aws
repo="$FR_E2E_REPO"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
: >"$work/samples.tsv"
: >"$work/ticks"

# sample: one describe-instances; a line per instance in samples.tsv
# (sample time, id, runner tag, state, LaunchTime) and the time in ticks.
sample() {
	local t out
	t="$(e2e_now)"
	out="$(aws ec2 describe-instances --filters Name=tag-key,Values=firerunner:runner --output json 2>"$work/err")" ||
		return 1
	jq -r --argjson t "$t" '.Reservations[]?.Instances[]? |
		[$t, .InstanceId, ([.Tags[]? | select(.Key == "firerunner:runner") | .Value][0] // ""), .State.Name,
		 (.LaunchTime // "" | if . == "" then "" else .[0:19] + "Z" | fromdateiso8601 end)] | @tsv' \
		<<<"$out" >>"$work/samples.tsv" || return 1
	echo "$t" >>"$work/ticks"
}

# jobs.jsonl: every non-skipped job of the runs fetched so far, with its run.
jobs_file() {
	local id
	: >"$work/jobs.jsonl"
	for id in $runs; do
		[ -f "$work/jobs-$id.json" ] || continue
		jq -c --argjson run "$id" '.jobs[] | select(.conclusion != "skipped") | . + {run: $run}' \
			"$work/jobs-$id.json" >>"$work/jobs.jsonl"
	done
}

# ended: every started job's instance has been seen stopped or terminated,
# or was never seen at all (it exists before its job starts, so it will not
# appear later; the report names it).
ended() {
	report | jq -s -e 'all(.[] | select(.summary | not) | select(.runner != null);
		.instance == null or .t.stopped != null or .t.terminated != null)' >/dev/null
}

report() {
	jobs_file
	jq -c -n --rawfile s "$work/samples.tsv" --rawfile k "$work/ticks" --slurpfile jobs "$work/jobs.jsonl" \
		--argjson interval "$interval" '
		def t: if . then fromdateiso8601 else null end;
		def d(a; b): if a and b then b - a else null end;
		($k | split("\n") | map(select(. != "") | tonumber)) as $ticks |
		([range(1; $ticks | length) as $i | $ticks[$i] - $ticks[$i - 1]] | max) as $gap |
		($s | split("\n") | map(select(. != "") | split("\t") |
			{t: (.[0] | tonumber), id: .[1], runner: .[2], state: .[3], launch: (if .[4] == "" then null else .[4] | tonumber end)})) as $rows |
		def prev(x): [$ticks[] | select(. < x)] | max;
		def seen(rows; st): [rows[] | select(.state == st) | .t] | min;
		def err(x): if x == null or prev(x) == null then null else x - prev(x) end;
		[$jobs[] |
			.runner_name as $r |
			(if ($r // "") == "" then [] else [$rows[] | select(.runner == $r)] end) as $mine |
			($mine | group_by(.id) | map({id: .[0].id, launch: (map(.launch) | max)}) | max_by(.launch) // null) as $inst |
			(if $inst then [$mine[] | select(.id == $inst.id)] else [] end) as $ir |
			{launch: $inst.launch, pending: seen($ir; "pending"), running: seen($ir; "running"),
			 job_start: (.started_at | t), job_end: (.completed_at | t),
			 shutting_down: seen($ir; "shutting-down"), stopping: seen($ir; "stopping"),
			 stopped: seen($ir; "stopped"), terminated: seen($ir; "terminated")} as $t |
			{run, job: .name, job_id: .id, conclusion, runner: (if ($r // "") == "" then null else $r end), instance: $inst.id,
			 interval_s: $interval, sample_gap_max_s: $gap, t: $t,
			 err_s: {pending: err($t.pending), running: err($t.running), shutting_down: err($t.shutting_down),
			         stopping: err($t.stopping), stopped: err($t.stopped), terminated: err($t.terminated)},
			 d: {launch_to_running_s: d($t.launch; $t.running), running_to_job_start_s: d($t.running; $t.job_start),
			     job_s: d($t.job_start; $t.job_end),
			     job_end_to_shutting_down_s: d($t.job_end; $t.shutting_down),
			     job_end_to_stopping_s: d($t.job_end; $t.stopping), job_end_to_stopped_s: d($t.job_end; $t.stopped),
			     job_end_to_terminated_s: d($t.job_end; $t.terminated),
			     shutting_down_to_terminated_s: d($t.shutting_down; $t.terminated)}}] as $lines |
		($lines[]),
		([$lines[] | select(.instance != null and .t.job_end != null)] | length) as $measured |
		{summary: true, jobs: ($lines | length), measured: $measured, few_samples: ($measured < 5),
		 interval_s: $interval, sample_gap_max_s: $gap,
		 failed: [$lines[] | select(.conclusion != "success")] | length,
		 unmatched: [$lines[] | select(.instance == null) | .job],
		 d: (reduce ($lines[0].d // {} | keys_unsorted[]) as $key ({};
			.[$key] = ([$lines[] | .d[$key] | numbers] | sort |
				if length == 0 then {n: 0, min: null, p50: null, max: null}
				else {n: length, min: .[0], p50: .[(length - 1) / 2 | floor], max: .[-1]} end)))}'
}

# finish RC: print the report, warn about what it cannot say, exit.
finish() {
	local out
	out="$(report)"
	printf '%s\n' "$out"
	if [ "$(tail -n 1 <<<"$out" | jq .few_samples)" = true ]; then
		e2e_log "warning: only $(tail -n 1 <<<"$out" | jq .measured) measured job(s); with N < 5 a p50 or max is an anecdote, not a distribution"
	fi
	tail -n 1 <<<"$out" | jq -r '.unmatched[]' | while read -r j; do
		e2e_log "no instance tagged firerunner:runner matched job '$j' (did it start, and on a firerunner tier?)"
	done
	if [ "$1" = 0 ] && [ "$(tail -n 1 <<<"$out" | jq '.failed + (.unmatched | length)')" != 0 ]; then
		exit 1
	fi
	exit "$1"
}

# A first sample before dispatching anything: missing credentials or
# permissions fail here, not after N billable runs are queued.
sample || e2e_die 2 "describe-instances failed: $(cat "$work/err")"
t0="$(e2e_now)"
# GitHub's clock and ours may differ by a few seconds.
since=$((t0 - 10))
runs="${given# }"
if [ -z "$given" ]; then
	ref_args=()
	[ -z "${FR_E2E_REF:-}" ] || ref_args=(--ref "$FR_E2E_REF")
	i=0
	while [ "$i" -lt "$n" ]; do
		gh workflow run "$workflow" --repo "$repo" "${ref_args[@]+"${ref_args[@]}"}" "${inputs[@]+"${inputs[@]}"}" >/dev/null ||
			e2e_die 1 "dispatching $workflow failed (run $((i + 1)) of $n)"
		i=$((i + 1))
	done
	e2e_log "dispatched $workflow x$n in $repo"
fi
e2e_log "sampling instance states every ${interval}s, GitHub every ${poll}s"

# poll_github: find the dispatched runs, then fetch each incomplete run's jobs.
pending="" started=0
poll_github() {
	local id run jobs still=""
	if [ -z "$runs" ]; then
		runs="$(gh api "repos/$repo/actions/workflows/$workflow/runs?event=workflow_dispatch&per_page=100" 2>"$work/err" |
			jq -r --argjson since "$since" --argjson n "$n" \
				'[.workflow_runs[] | select((.created_at | fromdateiso8601) >= $since)] | sort_by(.created_at) |
				 if length >= $n then .[-$n:][] | .id else empty end' | tr '\n' ' ')" || runs=""
		runs="${runs% }"
		[ -n "$runs" ] || return 0
		e2e_log "runs: $runs"
		pending="$runs"
	fi
	for id in $pending; do
		if ! run="$(gh api "repos/$repo/actions/runs/$id" 2>"$work/err")" ||
			! jobs="$(gh api "repos/$repo/actions/runs/$id/jobs?per_page=100" 2>"$work/err")"; then
			e2e_log "gh api (retrying): $(cat "$work/err")"
			still="$still $id"
			continue
		fi
		printf '%s\n' "$jobs" >"$work/jobs-$id.json"
		if [ "$started" = 0 ] && [ "$(jq '[.jobs[] | select(.status == "in_progress" or .status == "completed")] | length' <<<"$jobs")" -gt 0 ]; then
			started=1
			e2e_log "first job started after $(($(e2e_now) - t0))s"
		fi
		[ "$(jq -r .status <<<"$run")" = completed ] || still="$still $id"
	done
	pending="${still# }"
}
[ -z "$runs" ] || pending="$runs"

last_gh=$((t0 - poll)) complete_at=""
while :; do
	now="$(e2e_now)"
	sample || e2e_log "describe-instances (retrying): $(cat "$work/err")"
	if [ $((now - last_gh)) -ge "$poll" ]; then
		last_gh="$now"
		poll_github
	fi
	if [ -n "$runs" ] && [ -z "$pending" ]; then
		[ -n "$complete_at" ] || complete_at="$now"
		ended && finish 0
		if [ $((now - complete_at)) -ge "$shutdown_deadline" ]; then
			e2e_log "instances not stopped or terminated ${shutdown_deadline}s after the runs completed"
			finish 124
		fi
	else
		elapsed=$((now - t0))
		if [ "$started" = 0 ] && [ "$elapsed" -ge "$start_deadline" ]; then
			if [ -z "$runs" ]; then
				e2e_log "the $n dispatched runs did not all appear within ${start_deadline}s"
			else
				e2e_log "no job started within ${start_deadline}s: no runner picked them up."
				e2e_log "check firerunner's log:  scripts/e2e/remote-exec.sh -- journalctl -u firerunner -n 200 --no-pager"
			fi
			finish 3
		fi
		if [ "$elapsed" -ge "$run_deadline" ]; then
			e2e_log "runs still going after ${run_deadline}s: $pending"
			finish 124
		fi
	fi
	sleep "$interval"
done
