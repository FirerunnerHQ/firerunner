### Added

- An entitlement record can carry its plan's limits: `max_runners`, at most
  that many runners at once across an installation's tiers (the narrower of
  this and the tenants list's holds), and `tiers`, the only tiers it launches
  on. A change applies within 30 seconds while the installation runs, and
  stops no runner. `firerunner entitlement set` takes `--max-runners` and
  `--tiers`; `firerunner status` shows the plan's limits and the limit in
  force.
