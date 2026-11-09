### Added

- `cache-server --body-idle-timeout` (default 60s) ends a request whose body
  sends nothing for that long. A steady upload is never cut.
