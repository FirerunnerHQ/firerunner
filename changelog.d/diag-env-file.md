### Changed

- `firerunner status` and `doctor` read the daemon's settings from
  `/etc/firerunner/firerunner.env` when no `FR_*` variable is set, and say so,
  so run from a shell on the host they describe the running deployment rather
  than a default one. `--env-file FILE` reads another file, `--env-file none`
  none.
