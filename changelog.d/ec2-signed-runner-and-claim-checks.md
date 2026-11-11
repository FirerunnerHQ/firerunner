### Security

- The EC2 reaper deregisters a runner at GitHub only when the instance's
  runner tag is signed by this install: firerunner now tags each job instance
  `firerunner:runner-mac`, an HMAC under the `--ec2-pool-key-file` key. A
  running instance without a valid one is left to `--max-vm-lifetime`.
  Without a key file, a restarted firerunner deregisters no runner an earlier
  process launched; `doctor` and the startup log now warn of a missing key on
  any EC2 install, not only one with a pool.
- Before starting a pool instance for a job, firerunner checks that its image
  and EBS volumes are still the ones it was launched with (signed in the new
  `firerunner:pool-disk-mac` tag), that its security groups are exactly the
  tier's, its subnet one of the tier's, and that it has no instance profile
  and one network interface. One that differs is terminated, not started.
  **Upgrading discards existing pool instances once:** they carry no disk
  MAC, so they are terminated and the pool refills.
