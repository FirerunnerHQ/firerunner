### Fixed

- Daemons sharing a tenants list under `--tenant-leases` share one
  configuration, and so one install. Each but the first could not open the
  install's sessions, never served an installation of the list, and exited
  after two minutes, again and again. A shard now starts the list's
  installations before the install's own tiers, and stands by for the
  install as long as another shard holds its sessions: it opens them all
  together or holds none, launches nothing for the install meanwhile (no
  warm pool either), and takes it over once that shard stops.
  `firerunner status` shows a shard standing by.
- Two daemons creating one scale set at once no longer fail the one that
  loses the race: it takes the scale set the other made.

### Changed

- The daemon starts the installations of its tenants list before its own
  tiers, so they no longer wait for those to register. If its own tiers
  then fail to start, the tenants hand their busy runners on as at any exit.
- A tier's warm pool starts filling once every tier of its install or
  tenant has registered, rather than as each does.
