### Added

- With `FR_JOB_EVENTS_DSN` set, each job a runner runs now gets one usage
  event in the job store, within about 15 seconds of completing.
  - **What it holds:** the job's tenant, repository, workflow, tier, machine,
    and its run time in milliseconds.
  - **Written once:** its ID stays the same however often the job's events
    arrive.
  - **Reading them:** `firerunner usage --after CURSOR` prints the events in
    order, one JSON object a line, for billing or chargeback to read.
    `proprietary/jobstore/usage-event.schema.json` describes an event.

### Changed

- Job events, and the jobs and runners kept from them, now name their
  tenant. Two tenants' runners with the same name no longer share a row.
  Rows written before the upgrade belong to the install.
- The job store's migration is one-way: after it, an older firerunner can
  no longer write to the store. Do not roll back while keeping it.
