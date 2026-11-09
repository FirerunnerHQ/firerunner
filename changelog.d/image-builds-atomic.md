### Fixed

- The image builders write a new image beside `--out` and rename it into place
  only when it is complete, so a failed rebuild keeps the previous image instead
  of leaving a half-built one where firerunner boots from. `build-rootfs.sh`
  checks its downloads before bootstrapping, so a bad download fails in seconds.
- `build-toolcache.sh --python 3.12` picks the newest stable 3.12 release.
  It compared versions as text, so 3.12.9 beat 3.12.16, and an old release
  without a GitHub checksum then failed the build.
