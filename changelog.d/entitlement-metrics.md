### Added

- `firerunner status` counts, per tier, the jobs held since the daemon
  started, by what held them: the installation's own runner limit, its
  plan's, a tier its plan leaves out, a hold or a suspension (`held_jobs` in
  `--json`). Each entitlement record shows when the store last saw it
  written, and an installation launching on an expired record shows since
  when and how many times it has begun to (`fail_open_since`, `fail_opens`);
  the daemon logs a warning each time one begins.
