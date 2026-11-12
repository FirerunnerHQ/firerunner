### Fixed

- An installation removed from the tenants list while it was not being
  served (waiting to start again after a failure, or not served as none of
  its tiers is within its limits) now has its scale sets deleted too, by
  name, as one removed while serving does.
