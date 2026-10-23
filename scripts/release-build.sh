#!/usr/bin/env bash
# Build the release binaries and their checksums into DIR:
#   firerunner_linux_amd64, firerunner_linux_arm64,
#   firerunner-control-bootstrap.sh, SHA256SUMS
# Usage: scripts/release-build.sh VERSION DIR
# The control VM's user-data (deploy/aws/control.yaml) downloads these by
# exact name and refuses any file whose SHA256SUMS line does not match.
set -euo pipefail
version="${1:?usage: release-build.sh VERSION DIR}"
dir="${2:?usage: release-build.sh VERSION DIR}"
mkdir -p "$dir"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "$dir/firerunner_linux_$arch" ./cmd/firerunner
done
install -m 0755 deploy/aws/control-bootstrap.sh "$dir/firerunner-control-bootstrap.sh"
sum=(sha256sum)
command -v sha256sum >/dev/null || sum=(shasum -a 256)
(cd "$dir" && "${sum[@]}" firerunner_linux_amd64 firerunner_linux_arm64 firerunner-control-bootstrap.sh > SHA256SUMS)
cat "$dir/SHA256SUMS"
