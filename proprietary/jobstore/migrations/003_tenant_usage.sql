-- Each record names its tenant, the tenants list's id; '' is the install the
-- flags name. Runner names are unique only within a tenant.
ALTER TABLE job_events ADD COLUMN tenant text NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN tenant text NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN tenant text NOT NULL DEFAULT '';
ALTER TABLE runners DROP CONSTRAINT runners_pkey;
ALTER TABLE runners ADD PRIMARY KEY (tenant, runner);
CREATE INDEX jobs_tenant ON jobs (tenant, completed_at);

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
       EXTRACT(EPOCH FROM j.started_at - j.assigned_at)   AS supply_seconds,
       j.tenant
FROM jobs j
LEFT JOIN runners r ON r.tenant = j.tenant AND r.runner = j.runner;

-- The usage export: one event per job a runner ran, written in the
-- transaction that records the job's completion. A row is never updated or
-- deleted; a correction will be an event of its own. Readers page in
-- (xact, seq) order, and only below the oldest transaction still open, so
-- one that commits late is never skipped; they dedupe on event_id.
CREATE TABLE usage_events (
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    xact         xid8 NOT NULL DEFAULT pg_current_xact_id(),
    event_id     text NOT NULL UNIQUE,
    tenant       text NOT NULL,
    job_id       text NOT NULL,
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    payload      jsonb NOT NULL,
    PRIMARY KEY (xact, seq)
);
CREATE INDEX usage_events_tenant ON usage_events (tenant, recorded_at);
