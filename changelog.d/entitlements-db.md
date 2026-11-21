### Added

- `--entitlements db:` reads what each installation may run from the job
  store's new `entitlements` table, one row per installation, which billing
  writes in SQL; the table dates every write itself. `firerunner entitlement`
  writes and lists it with `--store db:`, sets up a database that has no
  table yet, and says when the database role may not write it. Hand the
  table to billing's role and the daemon's role reads it only.
