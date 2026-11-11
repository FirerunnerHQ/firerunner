### Added

- `--guardduty` (`FR_GUARDDUTY`) reads AWS GuardDuty's findings on the
  install's EC2 instances every 2 minutes, and flags for review the tenant whose
  machine a finding names, by its `firerunner:owner` tag, from
  `--guardduty-min-severity` (default 4, Medium) up. A flag never suspends.
  `firerunner status` shows it, `doctor` warns while any tenant is flagged, the
  log warns once per finding, and job events record it as `flagged`. Flags are
  kept beside `--tenant-state`, under its name with `-flags` added, with how far
  the daemon has read, so a restart neither repeats nor misses a finding.
- `firerunner tenant clear-flag ID` clears a flag once reviewed; `tenant list`
  shows flags beside suspensions; `tenant findings` lists the findings on an
  owner's instances and the tenant each names, and writes nothing.
- CloudFormation: the control VM reads GuardDuty findings while the region has
  a detector, and writes flags under `<prefix>/control/tenant-state-flags`;
  update the stack to give its role those grants.
