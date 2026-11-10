### Fixed

- cache-server refuses, with 400 and before it claims any space, a block list
  that names a block more than once or names one never uploaded. Before, a
  repeated block could claim, and evict other cache entries for, bytes that
  were never uploaded.
