### Added

- With `--tenants db:` and `--tenant-leases`, an installation removed from
  the list no longer leaves its scale sets and machines behind when no
  daemon served it at the time (its daemon crashed, it was placed on none,
  or no daemon ran), or when its daemon's deletion failed or lost the
  lease midway. The job store keeps each removal in a `removed_tenants`
  table until all the installation left is deleted, and the assigning
  daemon deletes it under the lease on its account. `firerunner status`
  lists the sweeps under way and planned.
