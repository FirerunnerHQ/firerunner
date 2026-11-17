### Changed

- `scripts/e2e/inventory.sh` lists and cleans up an owner's tenants'
  instances (`<owner>/<id>`) too, which a session left behind before.
- `scripts/e2e/control-vm.sh up` can bring up a shard: `--env` adds settings
  to the session's `control/env`, `--job-store-schema` gives it the shared job
  store on a schema of its own, `--tenant-owner` and `--pool-key-secret` set
  the stack's `TenantOwner` and `PoolKeySecretArn`, and `--share-prefix-of`
  runs a second session on the first's parameters.
