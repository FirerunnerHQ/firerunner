### Added

- The CloudFormation templates (`control.yaml`, `byoc-role.yaml`,
  `reaper.yaml`) take a `TenantOwner` parameter: when set, the control VM's
  role, the bring-your-own-cloud role and the reaper Lambda also cover
  instances tagged `<TenantOwner>/<id>`, and the control VM runs with
  `FR_TENANT_OWNER`. Daemons that share a tenants list under
  `--tenant-leases` each run in a stack of their own with one `TenantOwner`.

### Fixed

- An `FR_TENANTS` set in the stack's `control/env` (such as `db:`) is no
  longer overridden by the control VM's default, the `tenants` parameter.
