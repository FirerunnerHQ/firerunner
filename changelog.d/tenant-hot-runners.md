### Added

- An installation of the tenants list can keep hot runners per tier
  (`limits.min`, `{"tier": N}`, or its entitlement's in its place): booted,
  registered and idle, so its jobs start in 1-2 s rather than about 21 s. There
  are fewer in all than its runner limit, and none while it is held or
  suspended. A hot runner idle 2 hours is replaced, and its machine lives long
  enough for a job it takes to reach its limit. `firerunner entitlement set
  --min TIER=N,...` writes an entitlement's hot runners.
