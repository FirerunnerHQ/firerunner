#!/usr/bin/env bash
# pr-wait.sh — wait for a PR's checks, then report checks, failed logs and
# review state in one go, so nobody polls `gh pr checks` by hand.
#
# Usage: scripts/pr-wait.sh PR [--deadline S] [--repo OWNER/REPO]
#   PR          number, URL or branch, as gh accepts
#   --deadline  give up after S seconds (default 1800)
#   --repo      default: the repository of the current directory
#
# It polls until every check on the PR's head commit has finished, or stops
# early as soon as a required check fails (any check, when none is required).
# Then it prints JSON lines on stdout, one object each, keyed by "type":
#   check            name, workflow, conclusion, duration_s, url
#   failed_log       the failed job's log, trimmed to the lines that matter
#   review           review decision and the reviews (author, state)
#   thread           one per unresolved review thread: path, line, author, body
#   review_required  the diff touches cost or security paths and nobody but
#                    the author has reviewed it (AGENTS.md, review gate)
#   summary          the verdict, last
# Exit status: 0 all checks passed and no thread is unresolved, 1 a check
# failed or a thread is unresolved, 2 usage or PR not found, 124 the deadline
# passed. Progress goes to stderr. Needs gh (authenticated) and jq.
# Tuning: FR_PR_WAIT_POLL (10 s between polls), FR_PR_WAIT_LOG_LINES (60).
set -euo pipefail

usage() { sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; }
log() { echo "pr-wait: $*" >&2; }
die() {
	log "$2"
	exit "$1"
}

# Paths whose changes need an independent review before merge (AGENTS.md).
GATED_PATHS='^internal/(scheduler|ec2|jitserver|listener|cacheserver|jobidentity)/|^internal/provisioner/egress'

pr="" deadline=1800 repo_args=()
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--deadline)
		[ $# -ge 2 ] || die 2 "--deadline needs a value"
		deadline="$2"
		shift
		;;
	--repo)
		[ $# -ge 2 ] || die 2 "--repo needs a value"
		repo_args=(--repo "$2")
		shift
		;;
	-*) die 2 "unknown option $1 (see --help)" ;;
	*)
		[ -z "$pr" ] || die 2 "one PR only"
		pr="$1"
		;;
	esac
	shift
done
[ -n "$pr" ] || {
	usage >&2
	exit 2
}
poll="${FR_PR_WAIT_POLL:-10}"
log_lines="${FR_PR_WAIT_LOG_LINES:-60}"
for v in "$deadline" "$poll" "$log_lines"; do
	case "$v" in '' | *[!0-9]*) die 2 "not a number of seconds or lines: '$v'" ;; esac
done
command -v gh >/dev/null || die 2 "gh not found"
command -v jq >/dev/null || die 2 "jq not found"

err="$(mktemp)"
trap 'rm -f "$err"' EXIT

# ${repo_args[@]+...} keeps bash 3.2 from calling an empty array unbound.
ghr() { gh "$@" ${repo_args[@]+"${repo_args[@]}"}; }

info="$(ghr pr view "$pr" --json number,url,author,files 2>"$err")" ||
	die 2 "PR $pr not found: $(cat "$err")"
number="$(jq -r .number <<<"$info")"
url="$(jq -r .url <<<"$info")"
author="$(jq -r '.author.login // ""' <<<"$info")"
# https://github.com/OWNER/REPO/pull/N
owner="$(cut -d/ -f4 <<<"$url")"
name="$(cut -d/ -f5 <<<"$url")"

now() { date +%s; }
give_up=$(($(now) + deadline))
checks='[]' seen="" verdict=""

# required_names: the names of the PR's required checks, one per line; empty
# when the repository requires none.
required_names() {
	ghr pr checks "$number" --required --json name --jq '.[].name' 2>/dev/null || true
}

while :; do
	# gh pr checks exits 8 while checks are pending and 1 when one failed, even
	# with --json; only the JSON on stdout matters here.
	out="$(ghr pr checks "$number" --json name,workflow,bucket,state,link,startedAt,completedAt 2>"$err" || true)"
	if jq -e 'type == "array"' >/dev/null 2>&1 <<<"$out"; then
		checks="$out"
	elif grep -q "no checks reported" "$err"; then
		checks='[]'
	else
		log "gh pr checks: $(cat "$err")"
	fi

	while IFS= read -r line; do
		case "$seen" in *"|$line|"*) continue ;; esac
		seen="$seen|$line|"
		log "check $line"
	done < <(jq -r '.[] | "\(.name): \(.bucket)"' <<<"$checks")

	failed="$(jq -r '.[] | select(.bucket == "fail" or .bucket == "cancel") | .name' <<<"$checks")"
	pending="$(jq '[.[] | select(.bucket == "pending")] | length' <<<"$checks")"
	total="$(jq length <<<"$checks")"
	if [ -n "$failed" ]; then
		required="$(required_names)"
		if [ -z "$required" ] || grep -qxF -f <(printf '%s\n' "$required") <<<"$failed"; then
			verdict=check_failed
			break
		fi
	fi
	if [ "$total" -gt 0 ] && [ "$pending" = 0 ]; then
		if [ -n "$failed" ]; then verdict=check_failed; else verdict=success; fi
		break
	fi
	if [ "$(now)" -ge "$give_up" ]; then
		if [ "$total" = 0 ]; then
			log "no check reported on PR #$number within ${deadline}s"
		else
			log "$pending check(s) still pending after ${deadline}s"
		fi
		verdict=deadline
		break
	fi
	sleep "$poll"
done

jq -c '.[] | {type: "check", name, workflow, conclusion: .bucket, state,
	duration_s: (if (.startedAt // "") != "" and (.completedAt // "") != "" and (.completedAt | startswith("0001")) == false
		then ([(.completedAt | fromdateiso8601) - (.startedAt | fromdateiso8601), 0] | max) else null end),
	url: .link}' <<<"$checks"

# trim_log: the failed job's log, without the "job<TAB>step<TAB>" prefix,
# timestamps and colours; then the failing step only (from its last
# "##[group]Run" to the first "##[error]"); then the lines that look like
# failures with three lines after each, or the step's tail if none does.
trim_log() {
	sed -e 's/^[^	]*	[^	]*	//' -e 's/^[0-9T:.-]*Z //' -e $'s/\x1b\\[[0-9;]*m//g' |
		awk -v max="$log_lines" '
			/##\[error\]/ { line[n++] = $0; done = 1; exit }
			/^##\[group\]Run / { n = 0 }
			{ line[n++] = $0 }
			END {
				hit = 0
				for (i = 0; i < n; i++)
					if (line[i] ~ /(FAIL|panic:|[Ee]rror|fatal|not ok|##\[error\])/)
						for (j = i; j < n && j <= i + 3; j++) keep[j] = 1
				for (i = 0; i < n; i++) if (keep[i]) hit++
				start = (hit == 0 && n > max) ? n - max : 0
				shown = 0
				for (i = start; i < n && shown < max; i++)
					if (hit == 0 || keep[i]) { print line[i]; shown++ }
			}'
}

while IFS=$'\t' read -r cname link; do
	[ -n "$cname" ] || continue
	# https://github.com/OWNER/REPO/actions/runs/RUN/job/JOB
	run_id="$(sed -n 's|.*/actions/runs/\([0-9]*\)/job/.*|\1|p' <<<"$link")"
	job_id="$(sed -n 's|.*/job/\([0-9]*\).*|\1|p' <<<"$link")"
	if [ -z "$run_id" ] || [ -z "$job_id" ]; then
		jq -cn --arg c "$cname" --arg u "$link" '{type: "failed_log", check: $c, url: $u, lines: [], note: "not an Actions job; open the url"}'
		continue
	fi
	log "fetching the failed log of $cname (run $run_id, job $job_id)"
	if raw="$(ghr run view "$run_id" --job "$job_id" --log-failed 2>"$err")"; then
		trim_log <<<"$raw" | jq -cRn --arg c "$cname" --arg u "$link" --argjson r "$run_id" --argjson j "$job_id" \
			'{type: "failed_log", check: $c, run: $r, job: $j, url: $u, lines: [inputs]}'
	else
		jq -cn --arg c "$cname" --arg u "$link" --arg e "$(cat "$err")" '{type: "failed_log", check: $c, url: $u, lines: [], note: $e}'
	fi
done < <(jq -r '.[] | select(.bucket == "fail" or .bucket == "cancel") | [.name, .link] | @tsv' <<<"$checks")

# Reviews and threads: one PR, narrow fields. A wide query (reviews and
# authors over many PRs) exceeds GitHub's 500,000-node limit.
# shellcheck disable=SC2016 # GraphQL variables, not shell ones
query='query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewDecision
      reviews(last: 30) { nodes { author { login } state } }
      reviewThreads(first: 100) {
        nodes {
          isResolved isOutdated path line
          comments(first: 1) { nodes { author { login } body url } }
        }
      }
    }
  }
}'
unresolved=0 reviewers=0
if rv="$(gh api graphql -F owner="$owner" -F name="$name" -F number="$number" -f query="$query" 2>"$err")"; then
	p="$(jq -c '.data.repository.pullRequest' <<<"$rv")"
	jq -c '{type: "review", decision: .reviewDecision,
		reviews: [.reviews.nodes[] | {author: (.author.login // null), state}]}' <<<"$p"
	jq -c '.reviewThreads.nodes[] | select(.isResolved | not) | .comments.nodes[0] as $c |
		{type: "thread", path, line, outdated: .isOutdated, author: ($c.author.login // null),
		 body: (($c.body // "") | if length > 1000 then .[0:1000] + "…" else . end), url: $c.url}' <<<"$p"
	unresolved="$(jq '[.reviewThreads.nodes[] | select(.isResolved | not)] | length' <<<"$p")"
	reviewers="$(jq --arg a "$author" '[.reviews.nodes[] | select((.author.login // "") != $a)] | length' <<<"$p")"
else
	log "review state unavailable: $(cat "$err")"
	jq -cn --arg e "$(cat "$err")" '{type: "review", decision: null, reviews: null, note: $e}'
fi

gated="$(jq -r '.files[].path' <<<"$info" | grep -E "$GATED_PATHS" || true)"
review_required=false
if [ -n "$gated" ] && [ "$reviewers" = 0 ]; then
	review_required=true
	jq -cRn '{type: "review_required", paths: [inputs],
		hint: "cost or security paths changed and no independent review exists: request Copilot, or give a fresh reviewer agent the diff (not your conclusions), and answer its findings before merging"}' <<<"$gated"
	log "review required: this diff touches cost or security paths and has no independent review"
fi

if [ "$verdict" = success ] && [ "$unresolved" -gt 0 ]; then verdict=unresolved_threads; fi
jq -cn --argjson n "$number" --arg u "$url" --arg v "$verdict" --argjson c "$checks" \
	--argjson t "$unresolved" --argjson rr "$review_required" '{type: "summary", pr: $n, url: $u, verdict: $v,
	checks: {total: ($c | length), pass: ([$c[] | select(.bucket == "pass")] | length),
		fail: ([$c[] | select(.bucket == "fail" or .bucket == "cancel")] | length),
		pending: ([$c[] | select(.bucket == "pending")] | length)},
	unresolved_threads: $t, review_required: $rr}'

case "$verdict" in
success) exit 0 ;;
deadline) exit 124 ;;
*) exit 1 ;;
esac
