### Security

- `images/build-rootfs.sh` checks the `actions/runner` tarball against the
  SHA-256 in its GitHub release notes, and Node.js against the release's
  `SHASUMS256.txt`, before using them; a mismatch fails the build. It prints
  the version and checksums it used and keeps them in the image at
  `/etc/firerunner/downloads.sha256`. `--runner-sha256` and `--node-sha256`
  pin the checksums.
