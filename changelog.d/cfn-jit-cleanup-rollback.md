### Fixed

- The control stack's JIT name cleanup removes the record only while the stack
  is being deleted (or while the create of a stack owning its zone rolls back),
  not when CloudFormation rolls back an update that added the cleanup, with the
  stack's VM still serving that name. A slow Route 53 or CloudFormation call
  now fails the delete within 5 minutes instead of an hour.

### Security

- The control VM's role may change only its own JIT name's `A` record, by
  `UPSERT`, rather than any record in the zone. `JitName` and `ZoneName` must
  be DNS names without a trailing dot.
