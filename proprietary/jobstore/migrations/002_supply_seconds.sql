-- supply_seconds is the part of a job's wait that firerunner answers for, from
-- GitHub assigning it to the job starting. queued_at stays empty until something
-- reports it: GitHub's scale set messages carry no queue time.
CREATE OR REPLACE VIEW job_timeline AS
SELECT j.job_id, j.repository, j.workflow, j.job_name, j.event_name, j.run_id, j.tier, j.runner, j.result,
       r.instance_type, r.market, r.zone, r.path, r.gone_reason,
       j.queued_at, j.completed_at,
       EXTRACT(EPOCH FROM j.assigned_at - j.queued_at)    AS github_seconds,
       EXTRACT(EPOCH FROM r.served_at - r.launched_at)    AS boot_seconds,
       EXTRACT(EPOCH FROM r.listening_at - r.served_at)   AS runner_seconds,
       EXTRACT(EPOCH FROM r.running_at - r.listening_at)  AS pickup_seconds,
       EXTRACT(EPOCH FROM j.started_at - j.queued_at)     AS queue_seconds,
       EXTRACT(EPOCH FROM j.completed_at - j.started_at)  AS job_seconds,
       EXTRACT(EPOCH FROM j.started_at - j.assigned_at)   AS supply_seconds
FROM jobs j
LEFT JOIN runners r USING (runner);
