### Fixed

- Hosted onboarding registers an organization's scale sets in its own runner
  group alone, by id. They used to be registered by the group's name, so in an
  organization that already had a runner group of that name they went into that
  group, which onboarding never opens to the installation's repositories only.
  An installation is now not served until its group is ready, and `firerunner
  status` says why. A tenants list entry can name its group by id with
  `runner_group_id`.
