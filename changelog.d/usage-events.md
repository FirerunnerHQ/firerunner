### Added

- With `FR_JOB_EVENTS_DSN` set, each job a runner runs now gets one usage
  event in the job store when it completes. The event holds its tenant,
  repository, workflow, tier, machine and run time in milliseconds, and its
  ID stays the same however often the job's events arrive.
  `firerunner usage --after CURSOR` prints the events in order, one JSON
  object a line, for billing or chargeback to read;
  `proprietary/jobstore/usage-event.schema.json` describes an event.

### Changed

- Job events, and the jobs and runners kept from them, now name their
  tenant. Runners are told apart by tenant and name, so two tenants'
  runners of the same name no longer share a row. Rows written before the
  upgrade belong to the install.
