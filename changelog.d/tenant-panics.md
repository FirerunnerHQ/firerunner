### Changed

- A panic in one installation's start, stop or tiers' listeners no longer
  ends the daemon: that installation stops, drains and starts again by
  itself, as after any other failure, while the others keep serving.
  `firerunner status` counts each installation's restarts, panics and last
  error, and `firerunner doctor` warns while one has panicked
  (`tenant-panics`).
