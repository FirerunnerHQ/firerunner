### Added

- Release files carry signed build provenance: `gh attestation verify FILE
  --repo FirerunnerHQ/firerunner` checks a download came from the release
  workflow at its tag. Releases are immutable once published, and only
  repository admins can create, move or delete a `v*` tag.
