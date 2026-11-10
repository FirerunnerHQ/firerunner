### Security

- EC2 pool instances are signed: firerunner tags each one it launches
  `firerunner:pool-mac`, an HMAC of its owner, tier and instance ID, and after
  a restart it never starts, nor hands a JIT config to, a pool instance whose
  MAC is missing or wrong; it terminates it instead. The key comes from the
  new `--ec2-pool-key-file` (`FR_EC2_POOL_KEY_FILE`); `control.yaml` generates
  one in Secrets Manager and the control VM reads it at boot. Without a key,
  each process uses a random one and the warm pool is rebuilt after every
  restart. **Upgrading discards existing pool instances once:** ones launched
  by an older release carry no MAC, so they are terminated and the pool
  refills.
