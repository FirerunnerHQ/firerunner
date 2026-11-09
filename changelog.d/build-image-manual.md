### Fixed

- The `build-image` workflow's tool-cache job now builds a drive (Node, Go,
  Python and CodeQL by default, set with the `toolcache_tools` input); it
  passed no tools and always failed.

### Changed

- `build-image` runs on manual dispatch only. Its monthly schedule waited a day
  for a `[self-hosted, kvm]` runner and was cancelled; run it, or add a
  schedule, where such a runner serves your copy.
