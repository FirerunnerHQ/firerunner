### Added

- With `--tenants db:` and `--tenant-leases`, an installation removed from
  the list while no daemon served it (its daemon crashed, it was placed on
  none, or no daemon ran) no longer leaves its scale sets and machines
  behind. The job store keeps it in a `removed_tenants` table until a
  daemon has deleted them, and the assigning daemon does so under the
  lease on its account. `firerunner status` lists the ones it is deleting.
