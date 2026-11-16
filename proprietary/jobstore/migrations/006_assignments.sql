-- Which shard serves each GitHub account, written only by the shard that
-- holds the assigner's lease (the leases row '!assigner'). A shard asks for
-- the leases of the accounts assigned to it. tiers is how many tiers the
-- account's tenant lists: a shard's load.
CREATE TABLE assignments (
    account     text PRIMARY KEY,
    shard       text NOT NULL,
    tiers       integer NOT NULL,
    assigned_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX assignments_shard ON assignments (shard);

-- A shard stopping: the assigner moves its accounts to the others at once.
-- list_ok: whether the shard's last read of the tenants list applied; the
-- assigner puts no tenant on one whose did not.
ALTER TABLE shards ADD COLUMN draining boolean NOT NULL DEFAULT false;
ALTER TABLE shards ADD COLUMN list_ok boolean NOT NULL DEFAULT false;
