### Added

- `cache-server --s3-credentials-file` signs S3 requests with the credentials
  in a file instead of the default AWS chain, and the daemon's
  `--cache-role-arn` (`FR_CACHE_ROLE_ARN`) keeps such a file fresh from a
  role it assumes, written to `--cache-credentials-file`. `status --json`
  reports the refresh as `listeners.cache_credentials`, and `doctor` fails its
  `cache-credentials` check when the file is missing or expired or the latest
  refresh failed.

### Security

- With the control stack's `Cache=s3`, cache-server no longer uses the control
  VM's instance role. The stack adds a cache role holding only the bucket's
  prefix, which the daemon assumes and hands to cache-server in a file;
  cache-server cannot reach instance metadata, as with `Cache=disk`, and the
  control role loses its S3 access. Deploy this release before updating the
  stack, then refresh the VM.
