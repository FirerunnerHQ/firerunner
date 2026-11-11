### Added

- cache-server counts the connections its caps refuse, by cap, and the
  uploads and downloads `--body-idle-timeout` cuts, by direction, in `/stats`
  and `/metrics` (`firerunner_cache_conn_refused_total{cap}`,
  `firerunner_cache_idle_cuts_total{direction}`), and logs each kind at most
  once a minute. The EC2 JIT endpoint counts its refusals too.
  `firerunner status` shows both, and `firerunner doctor` warns for an hour
  after the latest refusal.

### Fixed

- `firerunner status`, `doctor` and local scrapers reach cache-server's
  `/stats` and `/metrics` even when guests fill its connection caps:
  connections from the host itself are no longer capped.
