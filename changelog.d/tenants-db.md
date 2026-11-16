### Added

- `--tenants db:` reads the tenants list from a `tenants` table in the job
  store's database (`FR_JOB_EVENTS_DSN`), one row an installation, so the
  list has no length limit. `firerunner tenant import FILE` writes a whole
  list there, checked first as the daemon checks one, and `firerunner tenant
  export` prints it.
