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
# for KEY; $STATE/ans/KEY.err makes the call fail with that text on stderr.
cat >"$TMP/bin/fake-answer" <<'FAKE'
#!/usr/bin/env bash
key="$(printf '%s' "$1" | tr -c 'A-Za-z0-9.' '_')"
count=$(($(cat "$STATE/count.$key" 2>/dev/null || echo 0) + 1))
echo "$count" >"$STATE/count.$key"
if [ -f "$STATE/ans/$key.err" ]; then cat "$STATE/ans/$key.err" >&2; exit 1; fi
n=$count
while [ "$n" -gt 0 ] && [ ! -f "$STATE/ans/$key.$n" ]; do n=$((n - 1)); done
[ "$n" -gt 0 ] || { echo "fake: no answer for $1" >&2; exit 1; }
cat "$STATE/ans/$key.$n"
FAKE
cat >"$TMP/bin/aws" <<'FAKE'
#!/usr/bin/env bash
echo "aws $*" >>"$STATE/calls"
case "$*" in
*"ssm send-command"*)
	for a in "$@"; do case "$a" in file://*) cp "${a#file://}" "$STATE/params.json" ;; esac; done
	fake-answer send-command
	;;
*"ssm get-command-invocation"*) fake-answer invocation ;;
*"ssm cancel-command"*) ;;
*"ec2 get-console-output"*"--latest"*) fake-answer console-latest ;;
*"ec2 get-console-output"*) fake-answer console ;;
*"ec2 describe-instances"*) fake-answer describe-instances ;;
*) echo "unexpected aws call: $*" >&2; exit 1 ;;
esac
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
	unset FR_E2E_WORKFLOW FR_E2E_REF FR_E2E_START_DEADLINE FR_E2E_RUN_DEADLINE FR_E2E_EXEC_DEADLINE FR_E2E_POLL AWS_PROFILE
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
PATH="$TMP/bin:$PATH" "$HERE/env.sh" >"$STATE/out" 2>&1 && echo 0 >"$STATE/rc" || echo $? >"$STATE/rc"
expect "env.sh: exits 0 when configured" "$(cat "$STATE/out")" rc_is 0

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

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
