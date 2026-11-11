### Fixed

- `firerunner tenant` reads the daemon's settings file for its state store
  when no `FR_*` variable is set, as `status` and `doctor` do, so on a control
  VM `firerunner tenant list` needs no `--state`; `--env-file` chooses another
  file or none.
