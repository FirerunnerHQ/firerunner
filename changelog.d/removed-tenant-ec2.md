### Fixed

- An installation removed from the tenants list no longer leaves EC2
  resources behind. Once its scale sets are deleted, the daemon ends what it
  left under its owner tag: its stopped pool, the pool's spot requests, and
  job instances no daemon tracks, such as runners a stop left running.
  Before, nothing ever ended them.
