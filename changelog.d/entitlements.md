### Added

- `--entitlements` (`FR_ENTITLEMENTS`) reads, every 30 seconds, what billing
  says each tenant may run: its account state (`trial`, `active`, `past_due`,
  `suspended`), its budget state (`ok`, `warning`, `capped`) and until when.
  A tenant with no record, a suspended account or a capped budget is held:
  its jobs wait, recorded as `held` with the reason, and run once a record
  lets them; running jobs go on. An expired record holds a trial or past-due
  tenant until billing renews it, and lets an active one go on launching
  (unless capped). The install is never held. `firerunner status` shows each
  tenant's record and what it allows; `doctor` warns while reads fail, while
  a tenant launches on an expired record, or has none. Off unless set.
- `firerunner entitlement set|clear|list` writes and lists those records, as
  billing would, in a file or SSM.
- CloudFormation: the control VM may read `<prefix>/control/entitlements`;
  set `FR_ENTITLEMENTS` in the `env` parameter to turn them on.
