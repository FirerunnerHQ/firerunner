### Added

- `firerunner tenant suspend ID --reason TEXT`, `tenant resume ID` and `tenant
  list` keep which tenants are suspended where the daemon's new
  `--tenant-state` (`FR_TENANT_STATE`) names: a JSON file, or `ssm:` and an SSM
  path. The daemon reads it at start and every 15 seconds; a suspended tenant's
  launches are held, and every runner it has is ended at once, busy or not. Its
  reaper ends one a restart left behind. `firerunner status` shows the reason
  and the time, and `doctor` warns while the states cannot be read.
- CloudFormation: the control VM reads suspensions from
  `<prefix>/control/tenant-state`; update the stack to give its role that read.
