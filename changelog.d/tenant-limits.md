### Added

- Each tenant in the `--tenants` catalog runs under limits: by default at most 2
  runners at once across its tiers, only tiers of at most 2 vCPU, and 30 minutes
  per job. Raise them in its entry's `limits` (`max_runners`, `max_vcpu`,
  `max_job_duration`; 0 lifts one). A job past the runner limit waits, and
  `firerunner status` shows why; a job past its duration has its runner ended;
  a tier above the vCPU limit is not served to that tenant, with a warning.
  `firerunner config check` shows each tenant's limits, and the job events
  record `held` jobs and `stopped` runners with the reason. The install's own
  tiers take limits from `--limit-runners`, `--limit-vcpu` and
  `--limit-job-duration`, and have none by default.

### Changed

- Upgrading with a `--tenants` catalog: a tenant entry without `limits` now
  runs under the defaults above, so a tenant on tiers of more than 2 vCPU stops
  being served them, and its jobs past 30 minutes end. Add `limits` to each
  entry that needs more before upgrading; `firerunner config check` notes every
  tenant that sets none.
