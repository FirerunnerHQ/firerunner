-- What each tenant may run, as billing last said (#87): one row a tenant,
-- its record as every --entitlements store holds it
-- (proprietary/entitlement.Record: account and budget state, valid until,
-- limits). Billing writes the rows; firerunner --entitlements db: reads
-- them. A row that does not parse holds its tenant, as with any store.
-- updated_at is set on every write, whoever writes, by the trigger.
--
-- The daemon creates the table with the rest, so it owns it. For the
-- daemon's role to read it only, hand it to billing's role:
--   ALTER TABLE entitlements OWNER TO <billing role>;
--   GRANT SELECT ON entitlements TO <daemon role>;
CREATE TABLE entitlements (
    tenant     text PRIMARY KEY CHECK (tenant <> ''),
    record     jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION entitlements_written() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END
$$;

CREATE TRIGGER entitlements_written BEFORE INSERT OR UPDATE ON entitlements
    FOR EACH ROW EXECUTE FUNCTION entitlements_written();
