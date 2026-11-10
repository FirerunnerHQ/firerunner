### Changed

- `images/build-ec2-ami.sh` and `images/layer-ec2-ami.sh` check the build
  provenance of the firerunner release they put on an image with
  `gh attestation verify`, as `deploy-control.sh` does, and the image installs
  only a download with the SHA-256 that was checked. A `latest` build is
  pinned to the release's tag. They now need `gh`, logged in;
  `FIRERUNNER_SKIP_ATTESTATION=1` skips the check. A binary from
  `--firerunner-url` is still taken on its `--firerunner-sha256` alone, and
  `--firerunner-sha256` without `--firerunner-url` is refused.
