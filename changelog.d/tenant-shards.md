### Added

- `firerunner tenant shards` (`--json`) shows, from the job store, the
  daemons that share a tenants list under `--tenant-leases`: each one's
  state, which serves or is assigned each installation and why one is not
  served, which assigns, the removals not yet cleaned up, and the problems
  to look at. It writes nothing.
