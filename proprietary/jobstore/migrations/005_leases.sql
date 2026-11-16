-- Daemons that serve tenants of the tenants table under leases (firerunner
-- --tenant-leases), and the leases. A lease is on a GitHub account (host and
-- organization), not a tenant id: an account's scale sets and sessions are
-- what two daemons must not share. Every time here is the database's.
-- owner is the shard's own (--owner), tenant_owner the tenants' (shared).
CREATE TABLE shards (
    id           text PRIMARY KEY,
    host         text NOT NULL,
    owner        text NOT NULL,
    tenant_owner text NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    heartbeat_at timestamptz NOT NULL DEFAULT now()
);

-- epoch counts the shards that have held the lease: it grows by one each
-- time another shard takes it.
CREATE TABLE leases (
    account    text PRIMARY KEY,
    shard      text NOT NULL,
    epoch      bigint NOT NULL DEFAULT 1,
    expires_at timestamptz NOT NULL
);
CREATE INDEX leases_shard ON leases (shard);
