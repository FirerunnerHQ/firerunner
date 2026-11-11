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
