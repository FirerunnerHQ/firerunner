### Added

- The tenants list (`--tenants`, `FR_TENANTS`) can be an SSM parameter
  (`ssm:/name`), and the daemon reads it again every 30 seconds: installations
  added start, removed ones finish their jobs and have their scale sets
  deleted, and changed ones restart as listed once their runners finish. A
  change to `max_runners` alone applies at once. A list that does not read or
  check changes nothing, and `firerunner doctor` warns (`tenant-catalog`).
- The CloudFormation control VM reads its tenants from
  `<prefix>/control/tenants` itself, so adding an installation needs no new VM.

### Changed

- GuardDuty findings flag any installation under the install's owner tag,
  including one just removed whose machines are still finishing their jobs.
