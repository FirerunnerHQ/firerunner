-- Every event as it arrived, by month: raw detail ages out by dropping a partition.
CREATE TABLE job_events (
    at          timestamptz NOT NULL,
    kind        text        NOT NULL,
    tier        text        NOT NULL DEFAULT '',
    runner      text        NOT NULL DEFAULT '',
    job_id      text        NOT NULL DEFAULT '',
    repository  text        NOT NULL DEFAULT '',
    run_id      bigint      NOT NULL DEFAULT 0,
    detail      jsonb       NOT NULL DEFAULT '{}'
) PARTITION BY RANGE (at);
CREATE INDEX job_events_runner ON job_events (runner, at);
CREATE INDEX job_events_job ON job_events (job_id, at);

-- A job as GitHub sees it. One row, kept current from its events.
CREATE TABLE jobs (
    job_id        text PRIMARY KEY,
    repository    text NOT NULL DEFAULT '',
    workflow      text NOT NULL DEFAULT '',
    job_name      text NOT NULL DEFAULT '',
    event_name    text NOT NULL DEFAULT '',
    run_id        bigint NOT NULL DEFAULT 0,
    tier          text NOT NULL DEFAULT '',
    runner        text NOT NULL DEFAULT '',
    queued_at     timestamptz,
    assigned_at   timestamptz,
    started_at    timestamptz,
    completed_at  timestamptz,
    result        text NOT NULL DEFAULT ''
);
CREATE INDEX jobs_repository ON jobs (repository, queued_at DESC);
CREATE INDEX jobs_runner ON jobs (runner);

-- A runner and its machine. One per job, as runners are ephemeral.
CREATE TABLE runners (
    runner         text PRIMARY KEY,
    tier           text NOT NULL DEFAULT '',
    instance       text NOT NULL DEFAULT '',
    instance_type  text NOT NULL DEFAULT '',
    market         text NOT NULL DEFAULT '',
    subnet         text NOT NULL DEFAULT '',
    zone           text NOT NULL DEFAULT '',
    path           text NOT NULL DEFAULT '',
    launched_at    timestamptz,
    served_at      timestamptz,
    listening_at   timestamptz,
    running_at     timestamptz,
    exited_at      timestamptz,
    exit_code      integer,
    gone_at        timestamptz,
    gone_reason    text NOT NULL DEFAULT ''
);

-- A job's whole story, stage by stage, in seconds.
CREATE VIEW job_timeline AS
SELECT j.job_id, j.repository, j.workflow, j.job_name, j.event_name, j.run_id, j.tier, j.runner, j.result,
       r.instance_type, r.market, r.zone, r.path, r.gone_reason,
       j.queued_at, j.completed_at,
       EXTRACT(EPOCH FROM j.assigned_at - j.queued_at)    AS github_seconds,
       EXTRACT(EPOCH FROM r.served_at - r.launched_at)    AS boot_seconds,
       EXTRACT(EPOCH FROM r.listening_at - r.served_at)   AS runner_seconds,
       EXTRACT(EPOCH FROM r.running_at - r.listening_at)  AS pickup_seconds,
       EXTRACT(EPOCH FROM j.started_at - j.queued_at)     AS queue_seconds,
       EXTRACT(EPOCH FROM j.completed_at - j.started_at)  AS job_seconds
FROM jobs j
LEFT JOIN runners r USING (runner);
