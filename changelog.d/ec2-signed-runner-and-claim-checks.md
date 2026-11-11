### Security

- The EC2 reaper deregisters a runner at GitHub only when the instance's
  runner tag is signed by this install: firerunner now tags each job instance
  `firerunner:runner-mac`, an HMAC under the `--ec2-pool-key-file` key. A
  running instance without a valid one, including one still running from a
  release before this, is left to `--max-vm-lifetime`. Without a key file, a
  restarted firerunner deregisters no runner an earlier process launched;
  `doctor` and the startup log now warn of a missing key on any EC2 install,
  not only one with a pool.
- Before handing a pool instance a job, firerunner checks that its image and
  EBS volumes are still the ones it was launched with (signed in the new
  `firerunner:pool-disk-mac` tag), that its security groups are exactly the
  tier's, its subnet one of the tier's, its type the one it was launched as,
  and that it has no instance profile, one network interface, no public
  address and IMDSv2-only metadata. One that differs is never started, and
  is terminated. **Upgrading discards existing pool instances once:** they
  carry no disk MAC, so they are terminated and the pool refills.
