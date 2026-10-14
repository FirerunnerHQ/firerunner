#!/usr/bin/env bash
# Build as the caller; run real KVM tests in a disposable network namespace.
set -euo pipefail
: "${FR_KERNEL:?set FR_KERNEL to an absolute vmlinux path}"
: "${FR_GOLDEN:?set FR_GOLDEN to the smoke rootfs built from this checkout}"
cd "$(dirname "$0")/../.."
work=$(mktemp -d /tmp/fr-e2e.XXXXXX)
ns="fr-e2e-${work##*.}"
priv=()
if [[ $EUID -ne 0 ]]; then priv=(sudo); fi
created=0
cleanup() {
  if [[ $created == 1 ]]; then "${priv[@]}" ip netns del "$ns"; fi
  rm -rf "$work"
}
trap cleanup EXIT
go test -c -tags e2e -o "$work/e2e.test" ./test/e2e/
"${priv[@]}" ip netns add "$ns"
created=1
"${priv[@]}" ip -n "$ns" link set lo up
"${priv[@]}" ip -n "$ns" link add ext0 type dummy
"${priv[@]}" ip -n "$ns" link set ext0 up
"${priv[@]}" ip netns exec "$ns" env \
  FR_REQUIRE_E2E=1 FR_KERNEL="$FR_KERNEL" FR_GOLDEN="$FR_GOLDEN" \
  FR_EXT_IFACE=ext0 TMPDIR=/tmp \
  "$work/e2e.test" -test.v -test.count=1 -test.timeout=5m "$@"
