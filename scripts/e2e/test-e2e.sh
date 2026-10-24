#!/usr/bin/env bash
# Tests for the scripts/e2e harness with fake `aws`, `gh`, `date` and `sleep`:
# no AWS account, GitHub token or network needed, and no real waiting. The
# fake clock starts at 1000 and only `sleep` advances it, so deadlines are
# exercised exactly. API answers are scripted per call (see `answer`).
# Portable to macOS's bash 3.2.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
REAL_DATE="$(command -v date)"
export REAL_DATE

# Every fake records its call in $STATE/calls. An answer for KEY is read from
# $STATE/ans/KEY.N, where N is the highest number not above this call's count
# for KEY; $STATE/ans/KEY.err makes every call fail with that text on stderr,
# and an answer starting with "!err " makes that call fail with the rest.
# With a second argument, a call with no answer succeeds silently.
cat >"$TMP/bin/fake-answer" <<'FAKE'
#!/usr/bin/env bash
key="$(printf '%s' "$1" | tr -c 'A-Za-z0-9.' '_')"
count=$(($(cat "$STATE/count.$key" 2>/dev/null || echo 0) + 1))
echo "$count" >"$STATE/count.$key"
if [ -f "$STATE/ans/$key.err" ]; then cat "$STATE/ans/$key.err" >&2; exit 1; fi
n=$count
while [ "$n" -gt 0 ] && [ ! -f "$STATE/ans/$key.$n" ]; do n=$((n - 1)); done
if [ "$n" -eq 0 ]; then
	[ -z "${2:-}" ] || exit 0
	echo "fake: no answer for $1" >&2
	exit 1
fi
case "$(head -n 1 "$STATE/ans/$key.$n")" in
"!err "*) sed '1s/^!err //' "$STATE/ans/$key.$n" >&2 && exit 254 ;;
esac
cat "$STATE/ans/$key.$n"
FAKE
# The fake aws answers by key (see fake-answer), except for SSM parameters,
# which live as files under $STATE/ssm/<name>, their types in $STATE/ssm-types.
cat >"$TMP/bin/aws" <<'FAKE'
#!/usr/bin/env bash
echo "aws $*" >>"$STATE/calls"
arg() { # arg OPTION ARGS...: the value after OPTION
	local want="$1" prev=""
	shift
	for a in "$@"; do
		[ "$prev" = "$want" ] && { printf '%s' "$a"; return; }
		prev="$a"
	done
}
case "$1 $2" in
"ssm send-command")
	for a in "$@"; do case "$a" in file://*) cp "${a#file://}" "$STATE/params.json" ;; esac; done
	fake-answer send-command
	;;
"ssm get-command-invocation") fake-answer invocation ;;
"ssm cancel-command") ;;
"ssm get-parameter")
	f="$STATE/ssm$(arg --name "$@")"
	[ -f "$f" ] || { echo "An error occurred (ParameterNotFound) when calling the GetParameter operation" >&2; exit 254; }
	cat "$f"
	;;
"ssm put-parameter")
	j="$(arg --cli-input-json "$@")"
	j="${j#file://}"
	n="$(jq -r .Name "$j")"
	mkdir -p "$(dirname "$STATE/ssm$n")"
	jq -r .Value "$j" >"$STATE/ssm$n"
	echo "$n $(jq -r .Type "$j")" >>"$STATE/ssm-types"
	;;
"ssm get-parameters-by-path")
	p="$(arg --path "$@")"
	(cd "$STATE/ssm" && find ".$p" -type f 2>/dev/null) | sed 's|^\.||' | jq -R . | jq -sc .
	;;
"ssm delete-parameters")
	on=0
	for a in "$@"; do
		case "$a" in --names) on=1 ;; --*) on=0 ;; *) [ "$on" = 0 ] || rm "$STATE/ssm$a" ;; esac
	done
	;;
"ec2 get-console-output")
	case "$*" in *--latest*) fake-answer console-latest ;; *) fake-answer console ;; esac
	;;
"ec2 describe-instances")
	case "$*" in
	*"tag:Name,Values=*control*"*) fake-answer control-vms ;;
	*tag:firerunner:runner,*) fake-answer runner-instances ;;
	*tag:firerunner:owner,*) fake-answer instances ;;
	*) fake-answer describe-instances ;; # lifecycle.sh: every firerunner:runner instance
	esac
	;;
"ec2 describe-spot-instance-requests") fake-answer spots ;;
"ec2 describe-volumes") fake-answer volumes ;;
"ec2 cancel-spot-instance-requests" | "ec2 terminate-instances" | "ec2 delete-volume") fake-answer "$2" optional ;;
"cloudformation describe-stacks")
	case "$*" in
	*"ParameterKey=='Owner'"*) fake-answer "owner-$(arg --stack-name "$@")" ;;
	*) fake-answer "stack-$(arg --stack-name "$@")" ;;
	esac
	;;
"cloudformation get-template-summary") fake-answer template-keys ;;
"cloudformation create-stack")
	f="$(arg --parameters "$@")"
	cp "${f#file://}" "$STATE/create-params.json"
	fake-answer create-stack optional
	;;
"cloudformation delete-stack") fake-answer delete-stack optional ;;
"cloudformation describe-stack-events") fake-answer stack-events ;;
"autoscaling describe-auto-scaling-groups") fake-answer asg ;;
"route53 list-resource-record-sets") fake-answer rrs ;;
"route53 change-resource-record-sets") arg --change-batch "$@" >"$STATE/rr-change.json" ;;
"s3 cp") fake-answer s3-cp optional ;;
"s3 presign") fake-answer presign ;;
*) echo "unexpected aws call: $*" >&2; exit 1 ;;
esac
FAKE
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
echo "curl $*" >>"$STATE/calls"
fake-answer curl
FAKE
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
echo "gh $*" >>"$STATE/calls"
case "$1 $2" in
"api "*) fake-answer "$2" ;;
"workflow run") fake-answer dispatch >/dev/null ;;
*) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
FAKE
cat >"$TMP/bin/date" <<'FAKE'
#!/usr/bin/env bash
if [ "$*" = "+%s" ]; then cat "$STATE/clock"; else exec "$REAL_DATE" "$@"; fi
FAKE
cat >"$TMP/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
echo $(($(cat "$STATE/clock") + $1)) >"$STATE/clock"
FAKE
chmod +x "$TMP/bin/"*

fails=0
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then echo "ok   $name"; else echo "FAIL $name: $detail"; fails=$((fails + 1)); fi
}
called() { grep -q -- "$1" "$STATE/calls" 2>/dev/null; }
not_called() { ! called "$1"; }
calls_of() { grep -c -- "$1" "$STATE/calls" 2>/dev/null || true; }
rc_is() { [ "$(cat "$STATE/rc")" = "$1" ]; }
err_has() { grep -q -- "$1" "$STATE/err"; }
last_json() { tail -n 1 "$STATE/out" | jq -e "$1" >/dev/null 2>&1; }
dump() { echo "rc=$(cat "$STATE/rc") out=$(cat "$STATE/out") err=$(cat "$STATE/err" 2>/dev/null)"; }
jqe() { jq -e "$@" >/dev/null; }

# fresh NAME: a new state directory, the clock at 1000, and a full configuration.
fresh() {
	STATE="$TMP/state-$1"
	mkdir -p "$STATE/ans"
	echo 1000 >"$STATE/clock"
	: >"$STATE/calls"
	export STATE AWS_REGION=test-region-1 FR_E2E_REPO=owner/repo FR_E2E_CONTROL_INSTANCE=i-0control
	# Never the .claude/e2e.env of the machine running the tests; a fixed owner.
	export FR_E2E_ENV_FILE=none FR_OWNER=e2e-test FR_E2E_ARTIFACTS="$STATE/artifacts"
	export AWS_CONFIG_FILE="$STATE/aws-config" AWS_SHARED_CREDENTIALS_FILE="$STATE/aws-credentials"
	unset FR_E2E_WORKFLOW FR_E2E_REF FR_E2E_START_DEADLINE FR_E2E_RUN_DEADLINE FR_E2E_EXEC_DEADLINE FR_E2E_POLL AWS_PROFILE
	unset FR_E2E_STACK FR_E2E_BUCKET FR_E2E_VERSION FR_E2E_TIER_SUFFIX FR_E2E_STACK_DEADLINE
}
# answer KEY N TEXT: what call N (and later ones) for KEY returns.
answer() { printf '%s\n' "$3" >"$STATE/ans/$(printf '%s' "$1" | tr -c 'A-Za-z0-9.' '_').$2"; }
answer_err() { printf '%s\n' "$2" >"$STATE/ans/$(printf '%s' "$1" | tr -c 'A-Za-z0-9.' '_').err"; }
# run SCRIPT ARGS...: run a harness script with the fakes first on PATH.
run() {
	local script="$1"
	shift
	PATH="$TMP/bin:$PATH" "$HERE/$script" "$@" >"$STATE/out" 2>"$STATE/err" </dev/null &&
		echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
}
iso() { jq -nr --argjson t "$1" '$t | todate'; }

# --- configuration -----------------------------------------------------------

fresh env-missing
unset AWS_REGION FR_E2E_REPO
PATH="$TMP/bin:$PATH" "$HERE/env.sh" >"$STATE/out" 2>&1 && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "env.sh: exits 2 when unconfigured" "$(dump)" rc_is 2
expect "env.sh: names AWS_REGION" "" grep -q "MISSING  AWS_REGION" "$STATE/out"
expect "env.sh: names FR_E2E_REPO" "" grep -q "MISSING  FR_E2E_REPO" "$STATE/out"

fresh env-ok
answer control-vms 1 '{"Reservations":[{"Instances":[{"InstanceId":"i-0other","State":{"Name":"running"},"LaunchTime":"2026-10-23T10:00:00+00:00","Tags":[{"Key":"Name","Value":"st-e2e-other-control"},{"Key":"firerunner:e2e-session","Value":"e2e-other"}]}]}]}'
PATH="$TMP/bin:$PATH" "$HERE/env.sh" >"$STATE/out" 2>&1 && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "env.sh: exits 0 when configured" "$(cat "$STATE/out")" rc_is 0
expect "env.sh: shows this session's owner" "" grep -q "FR_OWNER=e2e-test (set)" "$STATE/out"
expect "env.sh: lists who else uses the environment" "$(cat "$STATE/out")" grep -q "e2e-other i-0other running" "$STATE/out"

fresh no-repo
unset FR_E2E_REPO
run watch-run.sh 7
expect "watch-run: missing FR_E2E_REPO exits 2" "$(dump)" rc_is 2
expect "watch-run: says what is missing" "" err_has "missing configuration: FR_E2E_REPO"
expect "watch-run: no gh call without config" "" not_called "gh "

fresh no-region
unset AWS_REGION
run remote-exec.sh -- true
expect "remote-exec: missing AWS_REGION exits 2" "$(dump)" rc_is 2
expect "remote-exec: names AWS_REGION" "" err_has "export AWS_REGION="
expect "remote-exec: no aws call without config" "" not_called "aws "

fresh no-instance
unset FR_E2E_CONTROL_INSTANCE
run console.sh
expect "console: no instance exits 2" "$(dump)" rc_is 2
expect "console: says how to name one" "" err_has "FR_E2E_CONTROL_INSTANCE"

fresh bad-deadline
export FR_E2E_START_DEADLINE=5m
run watch-run.sh 7
expect "a non-numeric deadline exits 2" "$(dump)" rc_is 2
expect "names the bad deadline" "" err_has "FR_E2E_START_DEADLINE must be"

fresh bad-repo
export FR_E2E_REPO=just-a-name
run watch-run.sh 7
expect "FR_E2E_REPO must be OWNER/REPO" "$(dump)" rc_is 2

# --- remote-exec -------------------------------------------------------------

inv() { # inv STATUS CODE STDOUT STDERR
	jq -cn --arg s "$1" --argjson c "$2" --arg o "$3" --arg e "$4" \
		'{Status: $s, StatusDetails: $s, ResponseCode: $c, StandardOutputContent: $o, StandardErrorContent: $e}'
}

fresh exec-ok
answer send-command 1 cmd-1
answer_err invocation "InvocationDoesNotExist" # not yet registered...
# shellcheck disable=SC2016 # a literal $HOME must arrive unexpanded
run remote-exec.sh i-0abc -- printf '%s|' 'a&b=c' "it's" 'x y' '$HOME'
expect "remote-exec: retries until the deadline while the invocation is unknown" "$(dump)" rc_is 124
rm "$STATE/ans/invocation.err" "$STATE"/count.*
: >"$STATE/calls"
echo 1000 >"$STATE/clock"
answer invocation 1 "$(inv InProgress -1 '' '')"
answer invocation 3 "$(inv Success 0 'remote out' 'remote err')"
# shellcheck disable=SC2016 # a literal $HOME must arrive unexpanded
run remote-exec.sh i-0abc -- printf '%s|' 'a&b=c' "it's" 'x y' '$HOME'
expect "remote-exec: exit 0 on success" "$(dump)" rc_is 0
expect "remote-exec: prints stdout" "$(cat "$STATE/out")" [ "$(cat "$STATE/out")" = "remote out" ]
expect "remote-exec: prints stderr" "" err_has "remote err"
expect "remote-exec: targets the instance" "" called "send-command --instance-ids i-0abc"
expect "remote-exec: polled until done" "$(calls_of get-command-invocation)" [ "$(calls_of get-command-invocation)" = 3 ]
expect "remote-exec: server-side deadline" "$(cat "$STATE/params.json")" \
	jqe '.executionTimeout == ["300"]' "$STATE/params.json"
sent="$(jq -r '.commands[0]' "$STATE/params.json")"
expect "remote-exec: arguments arrive exactly as given" "$sent -> $(sh -c "$sent")" \
	[ "$(sh -c "$sent")" = "a&b=c|it's|x y|\$HOME|" ]

fresh exec-default-instance
answer send-command 1 cmd-1
answer invocation 1 "$(inv Success 0 '' '')"
run remote-exec.sh -- 'ls /tmp | head -n 1'
expect "remote-exec: defaults to FR_E2E_CONTROL_INSTANCE" "$(dump)" called "send-command --instance-ids i-0control"
expect "remote-exec: one argument is a shell script" "" jqe '.commands == ["ls /tmp | head -n 1"]' "$STATE/params.json"

fresh exec-stdin
answer send-command 1 cmd-1
answer invocation 1 "$(inv Success 0 '' '')"
printf 'echo one\necho two\n' >"$STATE/script"
PATH="$TMP/bin:$PATH" "$HERE/remote-exec.sh" <"$STATE/script" >"$STATE/out" 2>"$STATE/err" && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "remote-exec: reads a script from stdin" "$(dump)" jqe '.commands == ["echo one\necho two"]' "$STATE/params.json"

fresh exec-fail
answer send-command 1 cmd-1
answer invocation 1 "$(inv Failed 3 'partial' 'boom')"
run remote-exec.sh -- false
expect "remote-exec: propagates the exit status" "$(dump)" rc_is 3
expect "remote-exec: prints the failure's stderr" "" err_has boom

fresh exec-timedout
answer send-command 1 cmd-1
answer invocation 1 "$(inv TimedOut -1 '' '')"
run remote-exec.sh --deadline 30 -- tcpdump -i any
expect "remote-exec: a command killed on the instance exits 124" "$(dump)" rc_is 124
expect "remote-exec: --deadline reaches SSM" "" jqe '.executionTimeout == ["30"]' "$STATE/params.json"

fresh exec-hang
answer send-command 1 cmd-1
answer invocation 1 "$(inv InProgress -1 '' '')"
export FR_E2E_EXEC_DEADLINE=20
run remote-exec.sh -- sleep 1000
expect "remote-exec: gives up past its deadline with 124" "$(dump)" rc_is 124
expect "remote-exec: gives up near deadline + slack" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" -le $((1000 + 20 + 60 + 5)) ]
expect "remote-exec: cancels the command it abandons" "" called "cancel-command --command-id cmd-1"

fresh exec-send-fails
answer_err send-command "InvalidInstanceId"
run remote-exec.sh -- true
expect "remote-exec: send-command failure exits 1" "$(dump)" rc_is 1
expect "remote-exec: hints at SSM" "" err_has "SSM agent"
expect "remote-exec: no poll after a failed send" "" not_called get-command-invocation

fresh exec-usage
run remote-exec.sh i-0a i-0b -- true
expect "remote-exec: two instances is a usage error" "$(dump)" rc_is 2
run remote-exec.sh not-an-instance -- true
expect "remote-exec: rejects a non-instance ID" "$(dump)" rc_is 2
expect "remote-exec: no aws on usage errors" "" not_called "aws "

# --- console -----------------------------------------------------------------

fresh console-ok
answer console-latest 1 "$(for i in $(seq 1 200); do printf 'line %s\r\n' "$i"; done)"
run console.sh i-0abc -n 5
expect "console: exit 0" "$(dump)" rc_is 0
expect "console: tails N lines" "$(cat "$STATE/out")" [ "$(wc -l <"$STATE/out" | tr -d ' ')" = 5 ]
expect "console: last line, no CR" "$(tail -n 1 "$STATE/out" | od -c | head -n 1)" [ "$(tail -n 1 "$STATE/out")" = "line 200" ]

fresh console-fallback
answer_err console-latest "UnsupportedOperation"
answer console 1 "boot ok"
run console.sh i-0abc
expect "console: falls back without --latest" "$(dump)" [ "$(cat "$STATE/out")" = "boot ok" ]

fresh console-empty
answer console-latest 1 "None"
run console.sh i-0abc
expect "console: no output yet exits 1" "$(dump)" rc_is 1
expect "console: says it lags" "" err_has "no console output"

# --- watch-run ---------------------------------------------------------------

RUN=repos/owner/repo/actions/runs/7
JOBS="repos/owner/repo/actions/runs/7/jobs?per_page=100"
runj() { # runj STATUS CONCLUSION
	jq -cn --arg s "$1" --arg c "$2" --arg t "$(iso 1000)" \
		'{id: 7, status: $s, conclusion: (if $c == "" then null else $c end), created_at: $t, html_url: "https://example.invalid/run/7"}'
}
jobj() { # jobj NAME STATUS CONCLUSION
	jq -cn --arg n "$1" --arg s "$2" --arg c "$3" \
		'{jobs: [{name: $n, status: $s, conclusion: (if $c == "" then null else $c end), html_url: "https://example.invalid/job"}]}'
}

fresh watch-ok
answer "$RUN" 1 "$(runj queued '')"
answer "$JOBS" 1 "$(jobj selftest queued '')"
answer "$RUN" 2 "$(runj in_progress '')"
answer "$JOBS" 2 "$(jobj selftest in_progress '')"
answer "$RUN" 4 "$(runj completed success)"
answer "$JOBS" 4 "$(jobj selftest completed success)"
run watch-run.sh 7
expect "watch-run: exit 0 on success" "$(dump)" rc_is 0
expect "watch-run: JSON verdict" "$(cat "$STATE/out")" last_json '.verdict == "success" and .run == 7 and .failed_jobs == []'
expect "watch-run: logs job transitions" "" err_has "job selftest: in_progress"
expect "watch-run: logs each transition once" "" [ "$(grep -c "selftest: in_progress" "$STATE/err")" = 1 ]

fresh watch-job-fails
answer "$RUN" 1 "$(runj in_progress '')"
answer "$JOBS" 1 "$(jq -cn '{jobs: [{name: "a", status: "completed", conclusion: "failure", html_url: "u"}, {name: "b", status: "in_progress", conclusion: null}]}')"
run watch-run.sh 7
expect "watch-run: a failed job exits 1 at once" "$(dump)" rc_is 1
expect "watch-run: without waiting for the run" "" [ "$(calls_of 'runs/7$')" = 1 ]
expect "watch-run: names the failed job" "$(cat "$STATE/out")" last_json '.verdict == "job_failed" and .failed_jobs[0].name == "a"'
expect "watch-run: points at the failed log" "" err_has "--log-failed"

fresh watch-run-cancelled
answer "$RUN" 1 "$(runj completed cancelled)"
answer "$JOBS" 1 '{"jobs":[]}'
run watch-run.sh 7
expect "watch-run: a cancelled run exits 1" "$(dump)" rc_is 1
expect "watch-run: says how it ended" "" last_json '.verdict == "run_cancelled"'

fresh watch-no-runner
answer "$RUN" 1 "$(runj queued '')"
answer "$JOBS" 1 "$(jobj selftest queued '')"
export FR_E2E_START_DEADLINE=60
run watch-run.sh 7
expect "watch-run: no job started exits 3" "$(dump)" rc_is 3
expect "watch-run: at the start deadline, not before" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1060 ]
expect "watch-run: hints at the console" "" err_has "scripts/e2e/console.sh"
expect "watch-run: JSON on a stuck run" "" last_json '.verdict == "no_job_started"'

fresh watch-deadline
answer "$RUN" 1 "$(runj in_progress '')"
answer "$JOBS" 1 "$(jobj selftest in_progress '')"
export FR_E2E_RUN_DEADLINE=100 FR_E2E_POLL=10
run watch-run.sh 7
expect "watch-run: past the run deadline exits 124" "$(dump)" rc_is 124
expect "watch-run: at the run deadline" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1100 ]

fresh watch-api-flaky
answer_err "$RUN" "HTTP 502"
export FR_E2E_RUN_DEADLINE=20
run watch-run.sh 7
expect "watch-run: API errors retried until the deadline" "$(dump)" rc_is 124
expect "watch-run: logs the API error" "" err_has "HTTP 502"

fresh watch-not-found
answer_err "$RUN" "gh: Not Found (HTTP 404)"
run watch-run.sh 7
expect "watch-run: an unknown run exits 2" "$(dump)" rc_is 2

fresh watch-usage
run watch-run.sh abc
expect "watch-run: RUN_ID must be a number" "$(dump)" rc_is 2

# --- dispatch-and-measure ----------------------------------------------------

LIST="repos/owner/repo/actions/workflows/wf.yml/runs?event=workflow_dispatch&per_page=100"
mrun() { jq -cn --argjson id "$1" --arg s "$2" --arg c "$3" '{id: $id, status: $s, conclusion: (if $c == "" then null else $c end)}'; }
# mjobs CONCLUSION CREATED STARTED COMPLETED (epoch seconds; "" for none)
mjobs() {
	jq -cn --arg c "$1" --arg cr "$2" --arg st "$3" --arg co "$4" \
		'def t: if . == "" then null else (tonumber | todate) end;
		{jobs: [{name: "job", status: (if $co != "" then "completed" elif $st != "" then "in_progress" else "queued" end),
			conclusion: (if $c == "" then null else $c end), created_at: ($cr | t), started_at: ($st | t), completed_at: ($co | t)}]}'
}
runs_list() { # runs_list ID:CREATED...
	local items="" pair
	for pair in "$@"; do items="$items{\"id\":${pair%%:*},\"created_at\":\"$(iso "${pair#*:}")\"},"; done
	echo "{\"workflow_runs\":[${items%,}]}"
}

fresh measure-ok
answer dispatch 1 ok
answer "$LIST" 1 '{"workflow_runs":[]}'
answer "$LIST" 2 "$(runs_list 99:500 11:1001 12:1002)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 queued '')"
answer "repos/owner/repo/actions/runs/11/jobs?per_page=100" 1 "$(mjobs '' 1001 '' '')"
answer repos/owner/repo/actions/runs/11 2 "$(mrun 11 completed success)"
answer "repos/owner/repo/actions/runs/11/jobs?per_page=100" 2 "$(mjobs success 1001 1011 1041)"
answer repos/owner/repo/actions/runs/12 1 "$(mrun 12 completed success)"
answer "repos/owner/repo/actions/runs/12/jobs?per_page=100" 1 "$(mjobs success 1002 1032 1052)"
export FR_E2E_REF=feature
run dispatch-and-measure.sh wf.yml 2 -f tier=cloud
expect "measure: exit 0 when all succeed" "$(dump)" rc_is 0
expect "measure: dispatches N times" "$(calls_of "workflow run")" [ "$(calls_of "workflow run")" = 2 ]
expect "measure: passes ref and inputs" "" called "workflow run wf.yml --repo owner/repo --ref feature -f tier=cloud"
expect "measure: ignores runs from before the dispatch" "" not_called "runs/99"
expect "measure: per-job line" "$(cat "$STATE/out")" \
	jqe -s '.[] | select(.run == 11) | .queue_s == 10 and .run_s == 30 and .total_s == 40 and .conclusion == "success"' "$STATE/out"
expect "measure: summary p50/max" "$(tail -n 1 "$STATE/out")" \
	last_json '.summary and .runs == 2 and .jobs == 2 and .failed == 0 and .queue_s == {"p50": 10, "max": 30} and .run_s == {"p50": 20, "max": 30}'

fresh measure-job-fails
answer dispatch 1 ok
answer "$LIST" 1 "$(runs_list 11:1000)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 completed failure)"
answer "repos/owner/repo/actions/runs/11/jobs?per_page=100" 1 "$(mjobs failure 1000 1005 1010)"
run dispatch-and-measure.sh wf.yml 1
expect "measure: a failed job exits 1" "$(dump)" rc_is 1
expect "measure: counts it" "" last_json '.failed == 1 and .failed_runs == 1'

fresh measure-run-fails-no-jobs
answer dispatch 1 ok
answer "$LIST" 1 "$(runs_list 11:1000)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 completed startup_failure)"
answer "repos/owner/repo/actions/runs/11/jobs?per_page=100" 1 '{"jobs":[]}'
run dispatch-and-measure.sh wf.yml 1
expect "measure: a failed run with no jobs exits 1" "$(dump)" rc_is 1

fresh measure-no-runs
answer dispatch 1 ok
answer "$LIST" 1 '{"workflow_runs":[]}'
export FR_E2E_START_DEADLINE=30
run dispatch-and-measure.sh wf.yml 3
expect "measure: runs that never appear exit 3" "$(dump)" rc_is 3
expect "measure: says how many appeared" "" err_has "only 0 of 3 runs"

fresh measure-no-start
answer dispatch 1 ok
answer "$LIST" 1 "$(runs_list 11:1000)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 queued '')"
answer "repos/owner/repo/actions/runs/11/jobs?per_page=100" 1 "$(mjobs '' 1000 '' '')"
export FR_E2E_START_DEADLINE=50 FR_E2E_WORKFLOW=wf.yml
run dispatch-and-measure.sh 1
expect "measure: no job started exits 3" "$(dump)" rc_is 3
expect "measure: fails at the start deadline" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1050 ]
expect "measure: hints at the console" "" err_has "scripts/e2e/console.sh"
expect "measure: uses FR_E2E_WORKFLOW" "" called "workflow run wf.yml"

fresh measure-run-deadline
answer dispatch 1 ok
answer "$LIST" 1 "$(runs_list 11:1000)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 in_progress '')"
answer "repos/owner/repo/actions/runs/11/jobs?per_page=100" 1 "$(mjobs '' 1000 1001 '')"
export FR_E2E_RUN_DEADLINE=40
run dispatch-and-measure.sh wf.yml 1
expect "measure: past the run deadline exits 124" "$(dump)" rc_is 124
expect "measure: still prints a summary" "" last_json '.summary'

fresh measure-dispatch-fails
answer_err dispatch "HTTP 403"
run dispatch-and-measure.sh wf.yml 2
expect "measure: a failed dispatch exits 1" "$(dump)" rc_is 1
expect "measure: stops dispatching" "" [ "$(calls_of "workflow run")" = 1 ]

fresh measure-usage
for args in "wf.yml 0" "wf.yml x" "wf.yml 2 -f novalue" "1" "a b c"; do
	# shellcheck disable=SC2086 # split the case's words on purpose
	run dispatch-and-measure.sh $args
	expect "measure: rejects '$args'" "$(dump)" rc_is 2
done
expect "measure: no gh on usage errors" "" not_called "gh "

# --- lifecycle ---------------------------------------------------------------

# The probe sample is describe-instances call 1, at 1000; loop sample k (k >= 2)
# is call k, taken at 1000 + (k - 2) with the default 1 s interval.
# insts ID:RUNNER:STATE:LAUNCH...: a describe-instances answer.
insts() {
	local items="" q id runner state launch
	for q in "$@"; do
		IFS=: read -r id runner state launch <<<"$q"
		items="$items{\"InstanceId\":\"$id\",\"State\":{\"Name\":\"$state\"},\"LaunchTime\":\"$(iso "$launch" | sed 's/Z$/.000Z/')\",\"Tags\":[{\"Key\":\"Name\",\"Value\":\"$runner\"},{\"Key\":\"firerunner:runner\",\"Value\":\"$runner\"}]},"
	done
	echo "{\"Reservations\":[{\"Instances\":[${items%,}]}]}"
}
# ljobs CONCLUSION STARTED COMPLETED RUNNER...: one job per runner ("" for none).
ljobs() {
	local c="$1" st="$2" co="$3" items="" r
	shift 3
	for r in "$@"; do
		items="$items$(jq -cn --arg c "$c" --arg st "$st" --arg co "$co" --arg r "$r" \
			'def t: if . == "" then null else (tonumber | todate) end;
			{id: 1, name: ("job-" + $r), runner_name: (if $r == "" then null else $r end),
			 status: (if $co != "" then "completed" elif $st != "" then "in_progress" else "queued" end),
			 conclusion: (if $c == "" then null else $c end), started_at: ($st | t), completed_at: ($co | t)}'),"
	done
	echo "{\"jobs\":[${items%,}]}"
}
JOBS11="repos/owner/repo/actions/runs/11/jobs?per_page=100"
line_is() { jqe -s "$1" "$STATE/out"; }
not_err_has() { ! err_has "$1"; }

fresh life-ok
answer describe-instances 1 '{"Reservations":[]}'
answer describe-instances 2 "$(insts i-old:r-1:terminated:500 i-other:r-other:running:1000 i-1:r-1:pending:999)"
answer describe-instances 3 "$(insts i-old:r-1:terminated:500 i-other:r-other:running:1000 i-1:r-1:running:999)"
answer describe-instances 12 "$(insts i-old:r-1:terminated:500 i-other:r-other:running:1000 i-1:r-1:shutting-down:999)"
answer describe-instances 15 "$(insts i-old:r-1:terminated:500 i-other:r-other:terminated:1000 i-1:r-1:terminated:999)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 in_progress '')"
answer repos/owner/repo/actions/runs/11 3 "$(mrun 11 completed success)"
answer "$JOBS11" 1 "$(ljobs '' '' '' '')"
answer "$JOBS11" 2 "$(ljobs '' 1004 '' r-1)"
answer "$JOBS11" 3 "$(ljobs success 1004 1009 r-1)"
run lifecycle.sh --run 11
expect "lifecycle: exit 0 once the instance is terminated" "$(dump)" rc_is 0
expect "lifecycle: stops at the first terminated sample" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1013 ]
expect "lifecycle: dispatches nothing with --run" "" not_called "workflow run"
expect "lifecycle: matches the job's runner to its instance" "$(cat "$STATE/out")" \
	line_is '.[0] | .run == 11 and .runner == "r-1" and .instance == "i-1" and .conclusion == "success"'
expect "lifecycle: timeline" "$(head -n 1 "$STATE/out")" line_is '.[0].t == {launch: 999, pending: 1000, running: 1001,
	job_start: 1004, job_end: 1009, shutting_down: 1010, stopping: null, stopped: null, terminated: 1013}'
expect "lifecycle: durations" "$(head -n 1 "$STATE/out")" line_is '.[0].d | .launch_to_running_s == 2 and
	.running_to_job_start_s == 3 and .job_s == 5 and .job_end_to_shutting_down_s == 1 and
	.job_end_to_terminated_s == 4 and .shutting_down_to_terminated_s == 3 and .job_end_to_stopped_s == null'
expect "lifecycle: error bars from the sample gaps" "$(head -n 1 "$STATE/out")" \
	line_is '.[0] | .interval_s == 1 and .sample_gap_max_s == 1 and .err_s.running == 1 and .err_s.terminated == 1 and .err_s.pending == null'
expect "lifecycle: summary" "$(tail -n 1 "$STATE/out")" last_json '.summary and .jobs == 1 and .measured == 1 and .failed == 0 and
	.interval_s == 1 and .d.job_end_to_shutting_down_s == {n: 1, min: 1, p50: 1, max: 1}'
expect "lifecycle: N < 5 is flagged" "" last_json '.few_samples == true'
expect "lifecycle: and warned about" "" err_has "warning: only 1 measured job"
expect "lifecycle: GitHub polled every FR_E2E_POLL, not every sample" "$(calls_of "runs/11/jobs")" [ "$(calls_of "runs/11/jobs")" = 3 ]

fresh life-five
answer describe-instances 1 '{"Reservations":[]}'
answer describe-instances 2 "$(insts i-1:r-1:terminated:990 i-2:r-2:terminated:990 i-3:r-3:terminated:990 i-4:r-4:terminated:990 i-5:r-5:stopped:990)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 completed success)"
answer "$JOBS11" 1 "$(ljobs success 992 998 r-1 r-2 r-3 r-4 r-5)"
FR_E2E_INSTANCE_POLL=2 run lifecycle.sh --run 11
expect "lifecycle five: exit 0" "$(dump)" rc_is 0
expect "lifecycle five: a line per job" "" [ "$(wc -l <"$STATE/out" | tr -d ' ')" = 6 ]
expect "lifecycle five: not flagged" "$(tail -n 1 "$STATE/out")" last_json '.few_samples == false and .measured == 5 and .interval_s == 2'
expect "lifecycle five: no warning" "" not_err_has "warning"
expect "lifecycle five: a stopped instance counts as ended" "" line_is '.[4].t.stopped == 1000 and .[4].d.job_end_to_stopped_s == 2'
expect "lifecycle five: first-sample states have no error bar" "" line_is '.[0].err_s.terminated == null'

fresh life-dispatch
answer dispatch 1 ok
answer describe-instances 1 '{"Reservations":[]}'
answer describe-instances 3 "$(insts i-1:r-1:terminated:1000)"
answer "$LIST" 1 '{"workflow_runs":[]}'
answer "$LIST" 2 "$(runs_list 99:500 11:1001)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 completed failure)"
answer "$JOBS11" 1 "$(ljobs failure 1002 1003 r-1)"
run lifecycle.sh wf.yml 1 -f tier=cloud
expect "lifecycle dispatch: a failed job exits 1" "$(dump)" rc_is 1
expect "lifecycle dispatch: dispatched with inputs" "" called "workflow run wf.yml --repo owner/repo -f tier=cloud"
expect "lifecycle dispatch: ignores runs from before the dispatch" "" not_called "runs/99"
expect "lifecycle dispatch: still reports" "" last_json '.failed == 1'

fresh life-unmatched
answer describe-instances 1 '{"Reservations":[]}'
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 completed success)"
answer "$JOBS11" 1 "$(ljobs success 990 995 r-9)"
run lifecycle.sh --run 11
expect "lifecycle unmatched: exit 1" "$(dump)" rc_is 1
expect "lifecycle unmatched: does not wait for an instance it never saw" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1000 ]
expect "lifecycle unmatched: names the job" "" err_has "matched job 'job-r-9'"
expect "lifecycle unmatched: reports it" "" last_json '.unmatched == ["job-r-9"] and .measured == 0'

fresh life-no-shutdown
answer describe-instances 1 "$(insts i-1:r-1:running:990)"
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 completed success)"
answer "$JOBS11" 1 "$(ljobs success 992 995 r-1)"
export FR_E2E_SHUTDOWN_DEADLINE=20
run lifecycle.sh --run 11
expect "lifecycle: an instance that never ends exits 124" "$(dump)" rc_is 124
expect "lifecycle: at the shutdown deadline" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1020 ]
expect "lifecycle: still reports, terminated null" "" line_is '.[0].t.terminated == null and .[0].t.running == 1000'

fresh life-no-start
answer describe-instances 1 '{"Reservations":[]}'
answer repos/owner/repo/actions/runs/11 1 "$(mrun 11 queued '')"
answer "$JOBS11" 1 "$(ljobs '' '' '' '')"
export FR_E2E_START_DEADLINE=30
run lifecycle.sh --run 11
expect "lifecycle: no job started exits 3" "$(dump)" rc_is 3
expect "lifecycle: at the start deadline" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" = 1030 ]
expect "lifecycle: GitHub every 5 s while instances every 1 s" "$(calls_of "runs/11/jobs") $(calls_of describe-instances)" \
	[ "$(calls_of "runs/11/jobs"):$(calls_of describe-instances)" = 7:32 ]

fresh life-no-aws
answer_err describe-instances "UnauthorizedOperation"
answer dispatch 1 ok
run lifecycle.sh wf.yml 2
expect "lifecycle: a failing first sample exits 2" "$(dump)" rc_is 2
expect "lifecycle: says why" "" err_has "UnauthorizedOperation"
expect "lifecycle: before dispatching anything" "" not_called "workflow run"

fresh life-usage
for args in "wf.yml 0" "--run abc" "--run 11 wf.yml 2" "wf.yml 1 -i 0" "wf.yml 1 -i x" "a b c"; do
	# shellcheck disable=SC2086 # split the case's words on purpose
	run lifecycle.sh $args
	expect "lifecycle: rejects '$args'" "$(dump)" rc_is 2
done
expect "lifecycle: no calls on usage errors" "" not_called " "

# --- configuration files and the owner ---------------------------------------

# A throwaway repository with a worktree, holding a copy of the harness.
G="$TMP/repo"
gitq() { GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git -c user.name=t -c user.email=t@example.invalid "$@" >/dev/null 2>&1; }
gitq init -q -b main "$G"
mkdir -p "$G/scripts/e2e" "$G/.claude"
cp "$HERE"/*.sh "$G/scripts/e2e/"
gitq -C "$G" add scripts
gitq -C "$G" commit -q -m init
gitq -C "$G" worktree add -q -b feat/x "$G/.claude/worktrees/Feat_X.2"
W="$G/.claude/worktrees/Feat_X.2"
mkdir -p "$W/.claude"
cat >"$G/.claude/e2e.env" <<'EOF'
# shared by every worktree
AWS_REGION=main-region
AWS_PROFILE="main profile"
export FR_E2E_REPO='o/r'
FR_E2E_STACK=main-stack
FR_OWNER=someone-else
BOGUS=1
FR_E2E_REF=$HOME
EOF
echo "FR_E2E_STACK=wt-stack" >"$W/.claude/e2e.env"
# envrun DIR ARGS...: run DIR's env.sh with no e2e variables in the
# environment, except ENVRUN_SET (VAR=VALUE) if set.
envrun() {
	local d="$1"
	shift
	env -u AWS_REGION -u AWS_PROFILE -u FR_E2E_REPO -u FR_E2E_STACK -u FR_OWNER -u FR_E2E_ENV_FILE -u FR_E2E_REF \
		${ENVRUN_SET:+"$ENVRUN_SET"} PATH="$TMP/bin:$PATH" "$d/scripts/e2e/env.sh" "$@" >"$STATE/out" 2>"$STATE/err" &&
		echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
}

fresh env-files
answer control-vms 1 '{"Reservations":[]}'
echo "[main profile]" >"$AWS_SHARED_CREDENTIALS_FILE"
envrun "$W"
out_has() { grep -qF -- "$1" "$STATE/out"; }
out_re() { grep -qE -- "$1" "$STATE/out"; }
owner_of() { sed -n 's/^owner    FR_OWNER=//p' "$STATE/out"; }
not_out_owner() { ! grep -q FR_OWNER "$STATE/out"; }
expect "e2e.env: a worktree reads the main checkout's file" "$(dump)" out_has "set      AWS_REGION=main-region"
expect "e2e.env: its own file wins over the main checkout's" "" out_has "set      FR_E2E_STACK=wt-stack"
expect "e2e.env: quotes are stripped" "" out_has "set      AWS_PROFILE=main profile (in $AWS_SHARED_CREDENTIALS_FILE)"
expect "e2e.env: export prefix is accepted" "" out_has "set      FR_E2E_REPO=o/r"
expect "e2e.env: values are literal" "" out_has "set      FR_E2E_REF=\$HOME"
expect "e2e.env: a worktree ignores the shared file's FR_OWNER" "$(owner_of)" out_re "^owner    FR_OWNER=e2e-feat-x-2-[0-9a-f]{6}$"
expect "e2e.env: and says so" "" err_has "FR_OWNER is per session"
expect "e2e.env: other variables are ignored" "" err_has "BOGUS is not"
expect "e2e.env: tier suffix follows the owner" "" out_has "FR_E2E_TIER_SUFFIX=$(owner_of | sed 's/^e2e-//')"
first="$(owner_of)"
envrun "$W"
expect "owner: the same worktree always derives the same owner" "$first / $(owner_of)" [ "$(owner_of)" = "$first" ]
# Two worktrees whose names agree in the first 20 characters, the slug's length.
for t in issue-100-agent-harness-a issue-100-agent-harness-b; do
	gitq -C "$G" worktree add -q -b "$t" "$G/.claude/worktrees/$t"
done
envrun "$G/.claude/worktrees/issue-100-agent-harness-a"
own_a="$(owner_of)"
envrun "$G/.claude/worktrees/issue-100-agent-harness-b"
own_b="$(owner_of)"
expect "owner: worktrees with a common prefix get different owners" "$own_a / $own_b" \
	test -n "$own_a" -a "$own_a" != "$own_b"
expect "owner: a derived owner fits in 40 characters" "$own_a" [ "${#own_a}" -le 40 ]
ENVRUN_SET=AWS_REGION=from-env envrun "$W"
expect "e2e.env: the environment wins over the file" "$(dump)" out_has "set      AWS_REGION=from-env"
envrun "$W" --exports
expect "e2e.env: --exports prints export lines" "$(dump)" grep -qx "export AWS_PROFILE='main profile'" "$STATE/out"
expect "e2e.env: --exports never exports the owner" "" not_out_owner
expect "e2e.env: --exports output evaluates" "" [ "$(bash -c ". '$STATE/out'; printf '%s' \"\$AWS_PROFILE|\$FR_E2E_REF\"")" = "main profile|\$HOME" ]
envrun "$G"
expect "e2e.env: the main checkout's file never sets FR_OWNER, even for itself" "$(dump)" out_has "MISSING  FR_OWNER"
expect "e2e.env: and says so" "" err_has "FR_OWNER is per session"
: >"$AWS_SHARED_CREDENTIALS_FILE"
ENVRUN_SET=AWS_PROFILE=e2e envrun "$G"
expect "env.sh: a profile in neither AWS file is missing" "$(dump)" \
	out_has "MISSING  AWS_PROFILE=e2e: no [e2e] in $AWS_SHARED_CREDENTIALS_FILE, no [profile e2e] in $AWS_CONFIG_FILE"
echo "[profile e2e]" >"$AWS_CONFIG_FILE"
ENVRUN_SET=AWS_PROFILE=e2e envrun "$G"
expect "env.sh: finds a profile in the config file" "$(dump)" out_has "set      AWS_PROFILE=e2e (in $AWS_CONFIG_FILE)"
printf '[x]\n[e2e]\n' >"$AWS_SHARED_CREDENTIALS_FILE"
ENVRUN_SET=AWS_PROFILE=e2e envrun "$G"
expect "env.sh: finds a profile in the credentials file" "$(dump)" out_has "set      AWS_PROFILE=e2e (in $AWS_SHARED_CREDENTIALS_FILE)"
envrun "$G" --bogus
expect "env.sh: rejects an unknown argument" "$(dump)" rc_is 2
: >"$STATE/calls"
FR_OWNER="bad owner" PATH="$TMP/bin:$PATH" "$HERE/inventory.sh" >"$STATE/out" 2>"$STATE/err" && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "owner: a malformed FR_OWNER exits 2" "$(dump)" rc_is 2
env -u FR_OWNER PATH="$TMP/bin:$PATH" "$G/scripts/e2e/inventory.sh" --cleanup >"$STATE/out" 2>"$STATE/err" && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "owner: none is derived on main, which sessions share" "$(dump)" rc_is 2
expect "owner: says to set FR_OWNER" "" err_has "export FR_OWNER="
expect "owner: and touches nothing" "" not_called "aws "

# --- inventory ---------------------------------------------------------------

# inst ID STATE OWNER [LAUNCHED]: one instance as describe-instances shows it.
inst() {
	jq -cn --arg i "$1" --arg s "$2" --arg o "$3" --arg l "${4:-2026-10-23T10:00:00.000Z}" \
		'{InstanceId: $i, InstanceType: "m7a.large", State: {Name: $s}, LaunchTime: $l,
		Tags: [{Key: "firerunner:owner", Value: $o}, {Key: "firerunner:tier", Value: "cloud-test"}, {Key: "firerunner:runner", Value: ("runner-" + $i)}]}'
}
reservations() { jq -cn --argjson i "[$(
	IFS=,
	echo "$*"
)]" '{Reservations: [{Instances: $i}]}'; }
inventory_answers() {
	# The filter leaks another owner's instance: the script must drop it.
	answer instances 1 "$(reservations "$(inst i-0mine running e2e-test)" "$(inst i-0pool stopped e2e-test)" "$(inst i-0other running e2e-other)")"
	answer spots 1 '{"SpotInstanceRequests":[{"SpotInstanceRequestId":"sir-1","State":"active","Type":"persistent","InstanceId":"i-0pool","Tags":[{"Key":"firerunner:owner","Value":"e2e-test"}]},{"SpotInstanceRequestId":"sir-x","State":"open","Type":"persistent","Tags":[{"Key":"firerunner:owner","Value":"e2e-other"}]}]}'
	answer volumes 1 '{"Volumes":[{"VolumeId":"vol-used","State":"in-use","Tags":[{"Key":"firerunner:owner","Value":"e2e-test"}]},{"VolumeId":"vol-free","State":"available","Tags":[{"Key":"firerunner:owner","Value":"e2e-test"}]}]}'
	answer control-vms 1 '{"Reservations":[]}'
}
line_of() { grep -n -- "$1" "$STATE/calls" | head -n 1 | cut -d: -f1; }

fresh inventory-list
inventory_answers
run inventory.sh
expect "inventory: exit 0" "$(dump)" rc_is 0
expect "inventory: filters on the owner tag" "" called "tag:firerunner:owner,Values=e2e-test"
expect "inventory: lists only the owner's resources" "$(cat "$STATE/out")" \
	last_json '.owner == "e2e-test" and ([.instances[].id] == ["i-0mine", "i-0pool"]) and ([.spot_requests[].id] == ["sir-1"]) and ([.volumes[].id] == ["vol-used", "vol-free"])'
expect "inventory: lists without changing anything" "" not_called "terminate-instances"
run inventory.sh --owner e2e-other
expect "inventory: lists another owner" "$(dump)" last_json '[.instances[].id] == ["i-0other"]'

fresh inventory-dry-run
inventory_answers
run inventory.sh --cleanup
expect "cleanup: dry run by default" "$(dump)" last_json '.dry_run == true and .cancel_spot_requests == ["sir-1"] and .terminate_instances == ["i-0mine", "i-0pool"] and .delete_volumes == ["vol-free"]'
expect "cleanup: dry run cancels nothing" "" not_called "cancel-spot-instance-requests"
expect "cleanup: dry run terminates nothing" "" not_called "terminate-instances"

fresh inventory-cleanup
inventory_answers
run inventory.sh --cleanup --yes
expect "cleanup: exit 0" "$(dump)" rc_is 0
expect "cleanup: cancels the owner's spot requests" "" called "cancel-spot-instance-requests --spot-instance-request-ids sir-1$"
expect "cleanup: terminates the owner's instances" "" called "terminate-instances --instance-ids i-0mine i-0pool$"
expect "cleanup: cancels before it terminates" "$(cat "$STATE/calls")" \
	[ "$(line_of cancel-spot)" -lt "$(line_of terminate-instances)" ]
expect "cleanup: never terminates another owner's instance" "" not_called "i-0other"
expect "cleanup: never cancels another owner's spot request" "" not_called "sir-x"
expect "cleanup: deletes only unattached volumes" "" called "delete-volume --volume-id vol-free"
expect "cleanup: leaves attached volumes to their instance" "" not_called "vol-used"
expect "cleanup: reports what it did" "" last_json '.dry_run == false'

fresh inventory-cancel-fails
inventory_answers
answer cancel-spot-instance-requests 1 "!err UnauthorizedOperation"
run inventory.sh --cleanup --yes
expect "cleanup: a failed cancel exits 1" "$(dump)" rc_is 1
expect "cleanup: and terminates nothing" "" not_called "terminate-instances"

fresh inventory-not-mine
inventory_answers
run inventory.sh --owner e2e-other --cleanup --yes
expect "cleanup: refuses another owner" "$(dump)" rc_is 2
expect "cleanup: says how to adopt" "" err_has "set FR_OWNER=e2e-other"
expect "cleanup: no aws call for another owner" "" not_called "aws "

fresh inventory-live-vm
inventory_answers
answer control-vms 1 '{"Reservations":[{"Instances":[{"InstanceId":"i-0ctl","State":{"Name":"running"},"Tags":[{"Key":"Name","Value":"st-e2e-test-control"},{"Key":"firerunner:control","Value":"st-e2e-test"}]}]}]}'
answer owner-st-e2e-test 1 e2e-test
run inventory.sh --cleanup --yes
expect "cleanup: refuses while the owner's control VM is live" "$(dump)" rc_is 1
expect "cleanup: finds its owner through the stack" "" called "describe-stacks --stack-name st-e2e-test"
expect "cleanup: says to stop it first" "" err_has "control-vm.sh down"
expect "cleanup: terminates nothing while it is live" "" not_called "terminate-instances"

fresh inventory-owner-unknown
inventory_answers
answer control-vms 1 '{"Reservations":[{"Instances":[{"InstanceId":"i-0ctl","State":{"Name":"running"},"Tags":[{"Key":"Name","Value":"st-x-control"},{"Key":"firerunner:control","Value":"st-x"}]}]}]}'
answer owner-st-x 1 "!err AccessDenied"
run inventory.sh --cleanup --yes
expect "cleanup: a control VM whose owner cannot be read stops it" "$(dump)" rc_is 1
expect "cleanup: terminates nothing then" "" not_called "terminate-instances"
expect "cleanup: cancels nothing then" "" not_called "cancel-spot-instance-requests"
run inventory.sh
expect "inventory: listing still works without the owner" "$(dump)" last_json '.control_vms == []'

# --- watch-run consoles ------------------------------------------------------

fresh watch-console-failed-job
answer "$RUN" 1 "$(runj in_progress '')"
answer "$JOBS" 1 "$(jq -cn '{jobs: [{name: "a", status: "completed", conclusion: "failure", runner_name: "fr-cloud-1", html_url: "u"}]}')"
answer runner-instances 1 "$(reservations "$(inst i-0run terminated e2e-test)")"
answer console-latest 1 "$(printf 'boot\r\nfirerunner-ec2-run: JIT fetch failed\r\n')"
run watch-run.sh 7
f="$STATE/artifacts/run-7-i-0run.console"
expect "watch-run: still exits 1 on a failed job" "$(dump)" rc_is 1
expect "watch-run: finds the job's runner instance by name" "" called "tag:firerunner:runner,Values=fr-cloud-1"
expect "watch-run: saves its console" "$(ls "$STATE/artifacts" 2>&1)" grep -q "JIT fetch failed" "$f"
expect "watch-run: says where" "" err_has "to $f"
tail -n 1 "$STATE/out" >"$STATE/last"
# shellcheck disable=SC2016 # a jq program
expect "watch-run: the JSON names the console file" "" \
	jqe --arg f "$f" '.consoles == [{instance: "i-0run", runner: "runner-i-0run", file: $f}] and .failed_jobs[0].runner == "fr-cloud-1"' "$STATE/last"

fresh watch-console-no-runner
answer "$RUN" 1 "$(runj queued '')"
answer "$JOBS" 1 "$(jobj selftest queued '')"
answer instances 1 "$(reservations "$(inst i-0old running e2e-test "$(iso 900)")" "$(inst i-0new running e2e-test "$(iso 1010 | sed 's/Z$/+00:00/')")")"
answer console-latest 1 "no network"
export FR_E2E_START_DEADLINE=60
run watch-run.sh 7
expect "watch-run: no job started still exits 3" "$(dump)" rc_is 3
expect "watch-run: looks for this session's instances" "" called "tag:firerunner:owner,Values=e2e-test"
expect "watch-run: saves the consoles launched since the run" "" last_json '[.consoles[].instance] == ["i-0new"]'

fresh watch-console-no-aws
unset AWS_REGION
answer "$RUN" 1 "$(runj in_progress '')"
answer "$JOBS" 1 "$(jq -cn '{jobs: [{name: "a", status: "completed", conclusion: "failure", runner_name: "fr-1", html_url: "u"}]}')"
run watch-run.sh 7
expect "watch-run: without AWS it still reports the failure" "$(dump)" rc_is 1
expect "watch-run: and says it saved no console" "" err_has "not saving consoles"
expect "watch-run: with an empty consoles list" "" last_json '.consoles == []'

# --- control-vm --------------------------------------------------------------

TEMPLATE_KEYS="$(awk '/^Parameters:/ {p = 1; next} /^[A-Za-z]/ {p = 0} p && /^  [A-Za-z]+:$/ {gsub(/[ :]/, ""); print}' "$HERE/../../deploy/cloudformation/control.yaml" | jq -R . | jq -sc .)"
# ssm_seed NAME VALUE
ssm_seed() {
	mkdir -p "$(dirname "$STATE/ssm$1")"
	printf '%s\n' "$2" >"$STATE/ssm$1"
}
ssm_val() { cat "$STATE/ssm$1"; }
# base_stack [OWNER]: the shared stack and its parameters.
base_stack() {
	jq -cn --arg o "${1:-e2e-shared}" '{Stacks: [{StackName: "st", StackStatus: "UPDATE_COMPLETE",
		Parameters: [{ParameterKey: "Owner", ParameterValue: $o}, {ParameterKey: "VpcId", ParameterValue: "vpc-0aaa"},
			{ParameterKey: "SubnetIds", ParameterValue: "subnet-0aaa,subnet-0bbb"}, {ParameterKey: "ParameterPrefix", ParameterValue: "/fr/e2e"},
			{ParameterKey: "ReleaseRepo", ParameterValue: "acme/fr"}, {ParameterKey: "JitName", ParameterValue: "control"},
			{ParameterKey: "ZoneName", ParameterValue: "zone.internal"}, {ParameterKey: "HostedZoneId", ParameterValue: ""},
			{ParameterKey: "Retired", ParameterValue: "x"}],
		Outputs: [{OutputKey: "ParameterPrefix", OutputValue: "/fr/e2e"}, {OutputKey: "HostedZoneId", OutputValue: "Z0SHARED"},
			{OutputKey: "JitHost", OutputValue: "control.zone.internal"}]}]}'
}
# session_stack STATUS [TAG_OWNER]: this session's stack.
session_stack() {
	jq -cn --arg s "$1" --arg t "${2:-e2e-test}" '{Stacks: [{StackName: "st-e2e-test", StackStatus: $s,
		Tags: [{Key: "firerunner:e2e-session", Value: $t}],
		Parameters: [{ParameterKey: "Owner", ParameterValue: "e2e-test"}, {ParameterKey: "ParameterPrefix", ParameterValue: "/fr/e2e/sessions/e2e-test"},
			{ParameterKey: "HostedZoneId", ParameterValue: "Z0SHARED"}],
		Outputs: [{OutputKey: "ParameterPrefix", OutputValue: "/fr/e2e/sessions/e2e-test"},
			{OutputKey: "JitHost", OutputValue: "e2e-test.zone.internal"}]}]}'
}
NOSTACK="!err An error occurred (ValidationError) when calling the DescribeStacks operation: Stack with id st-e2e-test does not exist"
vm_fresh() {
	fresh "$1"
	export FR_E2E_STACK=st
	answer stack-st 1 "$(base_stack)"
	answer template-keys 1 "$TEMPLATE_KEYS"
	ssm_seed /fr/e2e/control/version v1.2.0
	ssm_seed /fr/e2e/control/sha256sums "aaa  firerunner_linux_arm64"
	ssm_seed /fr/e2e/control/env "FR_URL=https://example.invalid/org"
	ssm_seed /fr/e2e/control/app-private-key "SECRET-KEY-TEXT"
	ssm_seed /fr/e2e/control/tiers '[{"name":"cloud","labels":["cloud-arm"],"provisioner":"ec2","ec2":{"ami":"/fr/e2e/runner-ami"}},{"name":"lit","provisioner":"ec2","ec2":{"ami":"ami-0lit"}}]'
	ssm_seed /fr/e2e/runner-ami ami-0runner
	ssm_seed /fr/e2e/sessions/e2e-other/control/version v0.9.0
	answer asg 1 i-0ctl
	answer send-command 1 cmd-1
}

vm_fresh vm-up
answer stack-st-e2e-test 1 "$NOSTACK"
answer stack-st-e2e-test 2 "$(session_stack CREATE_IN_PROGRESS)"
answer stack-st-e2e-test 4 "$(session_stack CREATE_COMPLETE)"
answer asg 1 None
answer asg 2 i-0ctl
answer invocation 1 "$(inv Success 0 $'activating\n' '')"
answer invocation 2 "$(inv Success 0 $'active\nfirerunner v1.2.0\n' '')"
run control-vm.sh up
S=/fr/e2e/sessions/e2e-test
expect "up: exit 0 once firerunner is active" "$(dump)" rc_is 0
expect "up: reports the session" "$(tail -n 1 "$STATE/out")" \
	last_json '.owner == "e2e-test" and .stack == "st-e2e-test" and .instance == "i-0ctl" and .firerunner == "active" and .version == "v1.2.0" and .jit_host == "e2e-test.zone.internal" and .tiers == ["cloud-test", "lit-test"]'
expect "up: creates the session's own stack from control.yaml" "" called "create-stack --stack-name st-e2e-test --template-body file://.*deploy/cloudformation/control.yaml"
expect "up: tags the stack with the session's owner" "" called "Key=firerunner:e2e-session,Value=e2e-test"
expect "up: the stack's Owner is the session's" "$(cat "$STATE/create-params.json")" \
	jqe 'map({key: .ParameterKey, value: .ParameterValue}) | from_entries | .Owner == "e2e-test" and .ParameterPrefix == "/fr/e2e/sessions/e2e-test" and .JitName == "e2e-test" and .HostedZoneId == "Z0SHARED" and .ZoneName == "zone.internal" and .VpcId == "vpc-0aaa" and .SubnetIds == "subnet-0aaa,subnet-0bbb"' "$STATE/create-params.json"
expect "up: passes no parameter the template lacks" "" jqe 'all(.[]; .ParameterKey != "Retired")' "$STATE/create-params.json"
expect "up: suffixes tier names and labels" "$(ssm_val $S/control/tiers)" \
	jqe '[.[].name] == ["cloud-test", "lit-test"] and .[0].labels == ["cloud-arm-test"]' "$STATE/ssm$S/control/tiers"
expect "up: moves AMI parameters under the session prefix" "" \
	jqe '.[0].ec2.ami == "/fr/e2e/sessions/e2e-test/runner-ami" and .[1].ec2.ami == "ami-0lit"' "$STATE/ssm$S/control/tiers"
expect "up: copies the AMI parameter" "" [ "$(ssm_val $S/runner-ami)" = ami-0runner ]
expect "up: copies the app key as a SecureString" "" grep -qx "$S/control/app-private-key SecureString" "$STATE/ssm-types"
expect "up: never passes the key on a command line" "" not_called SECRET-KEY-TEXT
expect "up: pins the shared release and its checksums" "" [ "$(ssm_val $S/control/version)/$(ssm_val $S/control/sha256sums)" = "v1.2.0/aaa  firerunner_linux_arm64" ]
expect "up: needs no download for the shared release" "" not_called "curl "
expect "up: waits for firerunner to be active" "" [ "$(calls_of get-command-invocation)" = 2 ]

vm_fresh vm-up-not-ours
answer stack-st-e2e-test 1 "$(session_stack CREATE_COMPLETE e2e-someone)"
run control-vm.sh up
expect "up: refuses an existing stack made for another owner" "$(dump)" rc_is 1
expect "up: and does not wait on it" "" not_called "send-command"

vm_fresh vm-up-uppercase
export FR_OWNER=e2e-Test
run control-vm.sh up
expect "up: refuses an owner with capitals" "$(dump)" rc_is 2
expect "up: says it must be lower case" "" err_has "lower case"
expect "up: no aws call for it" "" not_called "aws "

vm_fresh vm-up-exists
answer stack-st-e2e-test 1 "$(session_stack CREATE_COMPLETE)"
answer invocation 1 "$(inv Success 0 $'active\nfirerunner v1.2.0\n' '')"
run control-vm.sh up
expect "up: an existing stack is only waited for" "$(dump)" rc_is 0
expect "up: and not created again" "" not_called create-stack

vm_fresh vm-up-version
answer stack-st-e2e-test 1 "$NOSTACK"
answer stack-st-e2e-test 2 "$(session_stack CREATE_COMPLETE)"
answer invocation 1 "$(inv Success 0 $'active\nfirerunner v2.0.0\n' '')"
answer curl 1 "bbb  firerunner_linux_amd64"
run control-vm.sh up --version v2.0.0
expect "up: --version pins that release's checksums" "$(dump)" called "curl -fsSL --max-time 60 https://github.com/acme/fr/releases/download/v2.0.0/SHA256SUMS"
expect "up: --version is what the VM installs" "" [ "$(ssm_val $S/control/version)" = v2.0.0 ]

vm_fresh vm-up-shared-owner
answer stack-st 1 "$(base_stack e2e-test)"
answer stack-st-e2e-test 1 "$NOSTACK"
run control-vm.sh up
expect "up: refuses the shared stack's own owner" "$(dump)" rc_is 2
expect "up: creates nothing then" "" not_called "create-stack"

vm_fresh vm-up-fails
answer stack-st-e2e-test 1 "$NOSTACK"
answer stack-st-e2e-test 2 "$(session_stack ROLLBACK_COMPLETE)"
answer stack-events 1 $'ControlGroup\tthe group failed'
run control-vm.sh up
expect "up: a failed stack exits 1" "$(dump)" rc_is 1
expect "up: shows why" "" err_has "the group failed"
expect "up: says how to clean up" "" err_has "control-vm.sh down"

vm_fresh vm-up-deadline
answer stack-st-e2e-test 1 "$NOSTACK"
answer stack-st-e2e-test 2 "$(session_stack CREATE_IN_PROGRESS)"
export FR_E2E_STACK_DEADLINE=60
run control-vm.sh up
expect "up: past FR_E2E_STACK_DEADLINE exits 124" "$(dump)" rc_is 124
expect "up: at the deadline" "$(cat "$STATE/clock")" [ "$(cat "$STATE/clock")" -le 1065 ]

vm_fresh vm-status
answer stack-st-e2e-test 1 "$NOSTACK"
run control-vm.sh status
expect "status: no stack is not an error" "$(dump)" rc_is 0
expect "status: says it is absent" "" last_json '.stack_status == "absent" and .instance == null'

vm_fresh vm-no-stack-config
unset FR_E2E_STACK
run control-vm.sh up
expect "control-vm: without FR_E2E_STACK exits 2" "$(dump)" rc_is 2
run control-vm.sh frobnicate
expect "control-vm: an unknown command exits 2" "$(dump)" rc_is 2

vm_fresh vm-list
answer control-vms 1 '{"Reservations":[{"Instances":[
	{"InstanceId":"i-0b","State":{"Name":"running"},"LaunchTime":"2026-10-23T11:00:00+00:00","Tags":[{"Key":"Name","Value":"st-control"},{"Key":"firerunner:control","Value":"st"}]},
	{"InstanceId":"i-0a","State":{"Name":"running"},"LaunchTime":"2026-10-23T10:00:00+00:00","Tags":[{"Key":"Name","Value":"st-e2e-x-control"},{"Key":"firerunner:control","Value":"st-e2e-x"},{"Key":"firerunner:e2e-session","Value":"e2e-x"}]},
	{"InstanceId":"i-0c","State":{"Name":"stopped"},"LaunchTime":"2026-10-23T12:00:00+00:00","Tags":[{"Key":"Name","Value":"hand-made-control"}]}]}]}'
answer owner-st 1 e2e-shared
run control-vm.sh list
expect "list: every live control VM with its owner, oldest first" "$(dump)" \
	last_json '[.[] | [.instance, .owner]] == [["i-0a", "e2e-x"], ["i-0b", "e2e-shared"], ["i-0c", null]]'

vm_fresh vm-down
answer stack-st-e2e-test 1 "$(session_stack CREATE_COMPLETE)"
answer stack-st-e2e-test 2 "$(session_stack DELETE_IN_PROGRESS)"
answer stack-st-e2e-test 3 "$NOSTACK"
inventory_answers
answer rrs 1 '[{"Name":"e2e-test.zone.internal.","Type":"A","TTL":10,"ResourceRecords":[{"Value":"10.0.0.5"}]}]'
ssm_seed $S/control/tiers '[]'
ssm_seed $S/control/version v1.2.0
ssm_seed $S/runner-ami ami-0runner
run control-vm.sh down
expect "down: exit 0" "$(dump)" rc_is 0
expect "down: deletes the session's stack" "" called "delete-stack --stack-name st-e2e-test"
expect "down: then cleans up the owner's runners" "$(cat "$STATE/calls")" \
	[ "$(line_of delete-stack)" -lt "$(line_of terminate-instances)" ]
expect "down: cancels spot requests before terminating" "" \
	[ "$(line_of cancel-spot)" -lt "$(line_of terminate-instances)" ]
expect "down: deletes the session's JIT name" "$(cat "$STATE/rr-change.json" 2>&1)" \
	jqe '.Changes == [{Action: "DELETE", ResourceRecordSet: {Name: "e2e-test.zone.internal.", Type: "A", TTL: 10, ResourceRecords: [{Value: "10.0.0.5"}]}}]' "$STATE/rr-change.json"
expect "down: deletes the session's parameters" "" test ! -e "$STATE/ssm$S/control/tiers" -a ! -e "$STATE/ssm$S/runner-ami"
expect "down: keeps the shared parameters" "" [ -e "$STATE/ssm/fr/e2e/control/tiers" ]
expect "down: keeps other sessions' parameters" "" [ -e "$STATE/ssm/fr/e2e/sessions/e2e-other/control/version" ]
expect "down: reports" "" last_json '.parameters_deleted == 3 and .jit_name_deleted == true and .cleanup.terminate_instances == ["i-0mine", "i-0pool"]'

vm_fresh vm-down-shared-owner
answer stack-st 1 "$(base_stack e2e-test)"
answer stack-st-e2e-test 1 "$NOSTACK"
inventory_answers
run control-vm.sh down
expect "down: refuses the shared stack's own owner" "$(dump)" rc_is 2
expect "down: cleans up nothing then" "" not_called "terminate-instances"
expect "down: deletes no stack then" "" not_called "delete-stack"

vm_fresh vm-down-not-ours
answer stack-st-e2e-test 1 "$(session_stack CREATE_COMPLETE e2e-someone)"
run control-vm.sh down
expect "down: refuses a stack not made for this owner" "$(dump)" rc_is 1
expect "down: deletes nothing then" "" not_called "delete-stack"
expect "down: terminates nothing then" "" not_called "terminate-instances"

vm_fresh vm-install
printf 'binary' >"$STATE/firerunner"
run control-vm.sh install "$STATE/firerunner"
expect "install: needs FR_E2E_BUCKET" "$(dump)" rc_is 2
export FR_E2E_BUCKET=bucket-placeholder
answer stack-st-e2e-test 1 "$(session_stack CREATE_COMPLETE)"
answer presign 1 "https://example.invalid/fr?X-Amz-Signature=abc&X-Amz-Expires=900"
answer invocation 1 "$(inv Success 0 $'active\nfirerunner dev-123\n' '')"
run control-vm.sh install "$STATE/firerunner"
expect "install: exit 0 when the new build is active" "$(dump)" rc_is 0
expect "install: uploads under the owner" "" called "s3 cp $STATE/firerunner s3://bucket-placeholder/e2e/e2e-test/firerunner"
sent="$(jq -r '.commands[0]' "$STATE/params.json")"
expect "install: fetches the presigned URL on the VM" "$sent" grep -qF "'https://example.invalid/fr?X-Amz-Signature=abc&X-Amz-Expires=900'" <<<"$sent"
expect "install: checks the checksum on the VM" "" grep -qF "$(printf binary | { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1)  /tmp/firerunner.new" <<<"$sent"
expect "install: reports the running version" "" last_json '.version == "dev-123" and .firerunner == "active"'

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
