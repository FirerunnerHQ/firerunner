### Fixed

- Daemons sharing a tenants list under `--tenant-leases` share one
  configuration, and so one install. Each but the first could not open the
  install's sessions, never served an installation of the list, and exited
  after two minutes, again and again. A shard now starts the list's
  installations before the install's own tiers, and stands by for the
  install's sessions as long as another shard holds them, taking them over
  when it stops. `firerunner status` shows a shard standing by.

### Changed

- The daemon starts the installations of its tenants list before its own
  tiers, so they no longer wait for those to register.
