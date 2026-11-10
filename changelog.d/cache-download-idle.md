### Changed

- cache-server's `--body-idle-timeout` (default 60s) now also ends a response
  the client stops reading for that long, a download above all; a transfer
  that keeps moving is never cut. One address may hold at most 64 connections
  to the cache server, and all of them together 4096.
