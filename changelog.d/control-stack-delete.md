### Fixed

- Deleting a `control.yaml` stack no longer fails with `HostedZoneNotEmpty`.
  The control VM's JIT name record, which the VM writes itself, is now
  deleted with the stack, after the VM and before the zone; in a zone passed
  as `HostedZoneId`, only that record is removed. A stack made before this
  needs updating to the new template first, or the record deleted by hand.
