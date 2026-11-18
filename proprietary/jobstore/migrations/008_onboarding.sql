-- The hosted App's installations, as onboarding (#170) has applied their
-- webhooks: one row an installation, whatever became of it. tenant is the
-- tenants list's id it was given; state is active, suspended, deleted, or
-- refused (with reason: a personal account, say). updated_at is the
-- installation's own, from the last event applied: an older one changes
-- nothing. deleted is final: GitHub never reuses an installation's id.
CREATE TABLE installations (
    installation_id bigint PRIMARY KEY,
    account_login   text NOT NULL,
    account_type    text NOT NULL,
    tenant          text NOT NULL DEFAULT '',
    state           text NOT NULL CHECK (state IN ('active', 'suspended', 'deleted', 'refused')),
    reason          text NOT NULL DEFAULT '',
    updated_at      timestamptz NOT NULL,
    applied_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX installations_tenant ON installations (tenant);

-- Webhook deliveries applied, by GitHub's delivery id: one delivered again
-- changes nothing. Kept for a week.
CREATE TABLE webhook_deliveries (
    delivery_id text PRIMARY KEY,
    event       text NOT NULL,
    outcome     text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
