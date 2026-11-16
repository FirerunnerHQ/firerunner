-- A tenant deleted from the tenants table, whoever deletes it, until a
-- daemon has deleted what it registered (its scale sets, by name) and
-- ended what it left on EC2. One listed again under its id is no longer
-- removed.
CREATE TABLE removed_tenants (
    id         text PRIMARY KEY,
    entry      jsonb NOT NULL,
    removed_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION tenants_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        INSERT INTO removed_tenants (id, entry) VALUES (OLD.id, OLD.entry)
            ON CONFLICT (id) DO UPDATE SET entry = excluded.entry, removed_at = now();
        RETURN OLD;
    END IF;
    DELETE FROM removed_tenants WHERE id = NEW.id;
    RETURN NEW;
END
$$;

CREATE TRIGGER tenants_removed AFTER INSERT OR DELETE ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_removed();
