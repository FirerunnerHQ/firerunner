### Added

- The tenants list (`--tenants`, `FR_TENANTS`) can be an SSM parameter
  (`ssm:/name`), and the daemon reads it again every 30 seconds: installations
  added start, removed ones finish their jobs and have their scale sets
  deleted, and changed ones restart as listed once their runners finish. A
  change to `max_runners` alone applies at once. A list that does not read or
  check changes nothing, and `firerunner doctor` warns (`tenant-catalog`); so
  does one that has disappeared, once a list has been read: removing every
  installation takes `[]`.
- The CloudFormation control VM reads its tenants from
  `<prefix>/control/tenants` itself, so adding an installation needs no new VM.

### Changed

- GuardDuty findings flag an installation listed now or still finishing its
  jobs after removal, and any installation under the owner tag until the
  first list is read.
