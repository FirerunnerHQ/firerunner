-- The tenants list, one row a tenant: its entry as a tenants list file holds
-- it (internal/config.Tenant). firerunner --tenants db: reads it.
CREATE TABLE tenants (
    id         text PRIMARY KEY CHECK (entry->>'id' = id),
    entry      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
