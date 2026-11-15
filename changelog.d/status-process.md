### Added

- `firerunner status` shows the daemon's own goroutines, Go heap and the
  memory Go has mapped, open files and CPU time since its start, and
  `status --json` has them under `listeners.process`. They are what sizing
  how many installations and tiers one daemon serves is measured from.
