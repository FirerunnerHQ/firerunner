### Fixed

- `--log-dir-max-mib` now holds while many runners write their console logs
  at once: it counts the bytes of logs still being written, removes only
  finished logs to make room, and stops a log with a closing line when the
  budget is spent. A log still open is no longer removed.
