### Added

- An installation of the tenants list can keep a stopped, pre-warmed pool per
  tier, so its jobs start in seconds. The pool is set by its entry's
  `limits.pool` (`{"tier": N}`) or by its entitlement's, which takes its place
  when the record names one. It is bounded by the tier's `max` and the runner
  limit, empty while the installation is held or suspended, and a change applies
  within 15 seconds with no restart. Before, a tenant's tiers never kept a pool.
- `firerunner entitlement set --pool TIER=N,...` writes an entitlement's pool.
- `firerunner status` shows each tier's stopped pool: ready of its target, and
  warming.
