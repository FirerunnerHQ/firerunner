### Changed

- With `--ec2-pool-key-file`, the EC2 reaper ends an instance only if a MAC
  under the key proves this install launched it (its pool or runner MAC, or
  its spot request's), and cancels a spot request only by its own MAC. The
  pool now signs each persistent spot request (`firerunner:request-mac`),
  including those of instances it takes in after the upgrade. What carries
  the owner tag but no such MAC is left alone: `firerunner status` lists it,
  and `firerunner doctor` warns (`ec2-reaper`). A pool instance whose MAC
  fails is no longer terminated, so changing the pool key leaves the old
  pool for you to end by hand.
- CloudFormation: `reaper.yaml` takes `PoolKeySecret` (`control.yaml`'s new
  output of that name); given it, the Lambda ends only instances a MAC
  proves the install's, and logs the rest as `unproven`.
