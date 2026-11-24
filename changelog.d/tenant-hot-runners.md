### Added

- An installation of the tenants list can keep hot runners per EC2 tier
  (`limits.min`, `{"tier": N}`, or its entitlement's in its place): booted,
  registered and idle, so its jobs start in 1-2 s rather than about 21 s. There
  are fewer in all than its runner limit, and none while it is held or
  suspended; a change applies within 15 seconds. A hot runner idle 30 minutes
  is replaced. `firerunner entitlement set --min TIER=N,...` writes an
  entitlement's hot runners.

### Changed

- The lifetime backstop of a tenants-list installation's machines is its job
  limit plus 45 minutes, up from 15, so that a hot runner that waited can still
  run its job to the limit.
