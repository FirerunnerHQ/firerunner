### Added

- `--entitlements db:` reads what each installation may run from the job
  store's new `entitlements` table, one row per installation, which billing
  writes in SQL; the table dates every write itself. `firerunner entitlement`
  writes and lists it with `--store db:`, and says when the database role
  may not write it, or the daemon has not set the database up yet. Hand the
  table to billing's role and the daemon's role reads it only.
