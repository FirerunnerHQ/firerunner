#!/usr/bin/env bash
#
# build-toolcache.sh — build a standalone, composable hosted tool-cache drive
# (ext4) for firerunner's `--toolcache` / `FR_TOOLCACHE` feature.
#
# firerunner can attach a read-only ext4 labelled `hostedtoolcache` to every
# microVM; the guest mounts it at /opt/hostedtoolcache and exports
# RUNNER_TOOL_CACHE, so `actions/setup-*` find their toolchain already on disk
# instead of downloading it per job (see the "Pre-seeded tool cache" section of
# the top-level README). This script is the operator-side builder for that
# drive: pick exactly the tools + versions your team uses and get one small,
# immutable image, decoupled from the base golden rootfs — so you can re-cut the
# cache (new versions, more tools) without rebuilding an OS image, and share the
# same drive across every tier.
#
# The layout mirrors GitHub's hosted tool cache so the stock `setup-*` actions
# hit it unmodified:
#     <tool>/<version>/x64/            # the tool tree
#     <tool>/<version>/x64.complete    # the marker @actions/tool-cache looks for
#
# The drive is a PURE ACCELERATOR: if a requested version is absent the setup-*
# action just downloads it and the job still passes. Nothing here is a
# correctness dependency.
#
# Runs on a Linux host. Node needs only curl/tar/mkfs.ext4 (no Docker); Go
# also needs jq. Python is fetched from actions/python-versions and relocated
# inside an ubuntu:24.04 container (matching the guest) so its shebangs/rpaths
# are correct, so `--python` additionally requires Docker and jq. CodeQL bakes
# the github/codeql-action bundle the same way runner-images does
# (self-contained, no Docker; needs jq).
#
# Every archive is checked on the host before it is unpacked (see
# lib/verify.sh): Node against the release's SHASUMS256.txt, Go against the
# go.dev download index, Python and CodeQL against the SHA-256 the GitHub API
# reports for the release asset. `--checksums FILE` pins them instead: lines
# of "SHA256  ARCHIVE-NAME", as sha256sum prints them; an archive it lists is
# checked against that line, any other as above. Each drive keeps the
# checksums it was built from in /downloads.sha256, in that format.
#
# Usage:
#   sudo ./build-toolcache.sh --out /var/tmp/fr-golden/toolcache.ext4 \
#     --node 22.22.2 --go 1.26.7                 # no Docker needed
#   sudo ./build-toolcache.sh --out toolcache.ext4 \
#     --node 20.18.0,22.22.2 --python 3.12       # Python => needs Docker
#   sudo ./build-toolcache.sh --out toolcache.ext4 --codeql latest
#     #  ^ bake the CodeQL bundle (accelerates github/codeql-action / SAST);
#     #    pass an explicit CLI version (e.g. --codeql 2.20.3) to pin it.
#   sudo ./build-toolcache.sh --out toolcache.ext4 --node 22.22.2 \
#     --checksums downloads.sha256              # rebuild from pinned checksums
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/verify.sh disable=SC1091
. "$HERE/lib/verify.sh"

OUT=""
ARCH="x64"
LABEL="hostedtoolcache"
SIZE_MB=""            # empty => sized from staged du + margin
MARGIN_PCT=25
UBUNTU_TAG="24.04"    # for the Python relocation container (match the guest)
CODEQL_VERSION=""     # "" => skip; "latest" => resolve like runner-images; or a CLI version
CHECKSUMS=""          # "" => every checksum from its upstream source (see above)
NODE_VERSIONS=() GO_VERSIONS=() PYTHON_VERSIONS=()

die() { echo "error: $*" >&2; exit 1; }
log() { echo ">> $*"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)       OUT="$2"; shift 2 ;;
    --node)      IFS=, read -ra _v <<<"$2"; NODE_VERSIONS+=("${_v[@]}"); shift 2 ;;
    --go)        IFS=, read -ra _v <<<"$2"; GO_VERSIONS+=("${_v[@]}"); shift 2 ;;
    --python)    IFS=, read -ra _v <<<"$2"; PYTHON_VERSIONS+=("${_v[@]}"); shift 2 ;;
    --codeql)    CODEQL_VERSION="$2"; shift 2 ;;
    --checksums) CHECKSUMS="$2"; shift 2 ;;
    --arch)      ARCH="$2"; shift 2 ;;
    --label)     LABEL="$2"; shift 2 ;;
    --size-mb)   SIZE_MB="$2"; shift 2 ;;
    --ubuntu-tag) UBUNTU_TAG="$2"; shift 2 ;;
    -h|--help)   sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d'; exit 0 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux"
[[ "$(id -u)" -eq 0 ]] || die "must run as root (mkfs/ownership)"
[[ -n "$OUT" ]] || die "--out is required"
[[ "$ARCH" == "x64" ]] || die "only --arch x64 is supported"
[[ -z "$CHECKSUMS" || -f "$CHECKSUMS" ]] || die "--checksums $CHECKSUMS: no such file"
for t in curl tar mkfs.ext4 e2label; do command -v "$t" >/dev/null || die "missing tool: $t"; done
if (( ${#NODE_VERSIONS[@]} + ${#GO_VERSIONS[@]} + ${#PYTHON_VERSIONS[@]} == 0 )) && [[ -z "$CODEQL_VERSION" ]]; then
  die "nothing to build: pass at least one of --node / --go / --python / --codeql"
fi
if (( ${#PYTHON_VERSIONS[@]} > 0 )); then
  command -v docker >/dev/null || die "--python requires docker (faithful actions/python-versions relocation)"
  docker info >/dev/null 2>&1 || die "--python requires a running docker daemon"
  command -v jq >/dev/null || die "--python requires jq (to pick the python-versions asset and its checksum)"
fi
if (( ${#GO_VERSIONS[@]} > 0 )); then
  command -v jq >/dev/null || die "--go requires jq (to read the checksum from the Go download index)"
fi
if [[ -n "$CODEQL_VERSION" ]]; then
  command -v jq >/dev/null || die "--codeql requires jq (to resolve the codeql-action bundle version)"
fi

STAGE="$(mktemp -d)"
DL="$(mktemp -d)"     # checked archives; never part of the drive
cleanup() { rm -rf "$STAGE" "$DL"; }
trap cleanup EXIT
export VERIFIED_LOG="$STAGE/downloads.sha256"
: >"$VERIFIED_LOG"

# fetch NAME URL LOOKUP...: downloads URL to $DL/NAME and checks it against
# NAME's line in --checksums or, failing that, the SHA-256 LOOKUP prints.
fetch() {
  local name="$1" url="$2" sha=""
  shift 2
  [[ -z "$CHECKSUMS" ]] || sha="$(awk -v f="$name" '{ n = $2; sub(/^\*/, "", n) } n == f { print $1; exit }' "$CHECKSUMS")"
  [[ -n "$sha" ]] || sha="$("$@")" || exit 1
  fetch_checked "$url" "$DL/$name" "$sha" "$name" || exit 1
}

# ---- Node: official nodejs.org static tarball ------------------------------
for v in ${NODE_VERSIONS[@]+"${NODE_VERSIONS[@]}"}; do
  dest="$STAGE/node/$v/$ARCH"
  log "node $v"
  mkdir -p "$dest"
  name="node-v${v}-linux-x64.tar.gz"
  fetch "$name" "https://nodejs.org/dist/v${v}/$name" node_sha256 "$v" "$name"
  tar -xzf "$DL/$name" --strip-components=1 -C "$dest" || die "node $v extract failed"
  [[ -x "$dest/bin/node" ]] || die "node $v: bin/node missing after extract"
  : > "$STAGE/node/$v/$ARCH.complete"
done

# ---- Go: official go.dev tarball -------------------------------------------
for v in ${GO_VERSIONS[@]+"${GO_VERSIONS[@]}"}; do
  dest="$STAGE/go/$v/$ARCH"
  log "go $v"
  mkdir -p "$dest"
  name="go${v}.linux-amd64.tar.gz"
  fetch "$name" "https://go.dev/dl/$name" go_sha256 "$name"
  # The archive's top-level dir is `go/`; strip it so bin/go lands at $dest/bin.
  tar -xzf "$DL/$name" --strip-components=1 -C "$dest" || die "go $v extract failed"
  [[ -x "$dest/bin/go" ]] || die "go $v: bin/go missing after extract"
  : > "$STAGE/go/$v/$ARCH.complete"
done

# ---- Python: actions/python-versions, relocated in an ubuntu container ------
# The python-versions release tarballs are prebuilt for a specific Ubuntu and
# ship a setup.sh that relocates the interpreter into $AGENT_TOOLSDIRECTORY with
# correct shebangs + a .complete marker. Run it inside ubuntu:${UBUNTU_TAG} so
# the layout matches the microVM guest exactly. The tarballs are picked and
# checked here, on the host; the container only unpacks them.
if (( ${#PYTHON_VERSIONS[@]} > 0 )); then
  PY_RELEASES="https://github.com/actions/python-versions/releases/download/"
  manifest="$(curl -fsSL --retry 3 https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json)" \
    || die "could not read the python-versions manifest"
  mkdir -p "$DL/python"
  for want in "${PYTHON_VERSIONS[@]}"; do
    # Newest release whose version starts with the requested prefix and that
    # ships a linux / ${UBUNTU_TAG} / x64 asset.
    url="$(printf '%s' "$manifest" | jq -r --arg v "$want" --arg pv "$UBUNTU_TAG" '
      [ .[]
        | select(.version | startswith($v))
        | . as $rel
        | $rel.files[]
        | select(.platform=="linux" and .arch=="x64" and .platform_version==$pv)
        | {version: $rel.version, url: .download_url} ]
      | sort_by(.version) | last | .url // empty')"
    [[ -n "$url" ]] || die "no python-versions asset for $want on ubuntu $UBUNTU_TAG x64"
    # Only a python-versions release asset has a checksum to check it against.
    [[ "$url" == "$PY_RELEASES"*/* ]] || die "python $want: the manifest points outside $PY_RELEASES: $url"
    log "python asset: $url"
    tag="${url#"$PY_RELEASES"}" tag="${tag%%/*}" name="${url##*/}"
    fetch "$name" "$url" github_asset_sha256 actions/python-versions "$tag" "$name"
    mv "$DL/$name" "$DL/python/$name"
  done
  log "python ${PYTHON_VERSIONS[*]} (via ubuntu:${UBUNTU_TAG} container)"
  docker run --rm -e AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    -v "$STAGE:/opt/hostedtoolcache" -v "$DL/python:/dl:ro" ubuntu:"${UBUNTU_TAG}" bash -euo pipefail -c '
      export DEBIAN_FRONTEND=noninteractive
      apt-get update >/dev/null
      apt-get install -y --no-install-recommends ca-certificates >/dev/null
      for tgz in /dl/*.tar.gz; do
        tmp="$(mktemp -d)"
        tar -xzf "$tgz" -C "$tmp"
        ( cd "$tmp" && bash ./setup.sh )
        rm -rf "$tmp"
      done
    ' || die "python relocation failed"
fi

# ---- CodeQL: bake the github/codeql-action bundle, runner-images style ------
# Mirrors actions/runner-images install-codeql-bundle.sh exactly so the stock
# github/codeql-action finds it the same way it does on ubuntu-latest:
#   CodeQL/<cliVersion>/x64/codeql/     (the bundle)
#   CodeQL/<cliVersion>/x64/pinned-version   (use this bundle authoritatively)
#   CodeQL/<cliVersion>/x64.complete    (tool-cache marker)
if [[ -n "$CODEQL_VERSION" ]]; then
  ver="$CODEQL_VERSION"
  if [[ "$ver" == "latest" ]]; then
    log "resolving the CodeQL bundle version pinned by the latest codeql-action"
    major="$(curl -fsSL https://api.github.com/repos/github/codeql-action/releases \
      | jq -r '.[].tag_name' | grep -E '^v[0-9]' | sort -rV | head -n1 | sed -E 's/^v([0-9]+).*/\1/')"
    [[ -n "$major" ]] || die "could not resolve the codeql-action major version"
    ver="$(curl -fsSL "https://raw.githubusercontent.com/github/codeql-action/v${major}/src/defaults.json" | jq -r '.cliVersion')"
    [[ -n "$ver" && "$ver" != "null" ]] || die "could not resolve the CodeQL cliVersion"
  fi
  log "codeql $ver"
  dest="$STAGE/CodeQL/$ver/$ARCH"
  mkdir -p "$dest"
  # The linux64 bundle is self-contained (its own runtimes + query packs); no
  # host toolchain or Docker needed. Saved under a versioned name, so a
  # --checksums line names one bundle.
  name="codeql-bundle-v${ver}-linux64.tar.gz"
  fetch "$name" \
    "https://github.com/github/codeql-action/releases/download/codeql-bundle-v${ver}/codeql-bundle-linux64.tar.gz" \
    github_asset_sha256 github/codeql-action "codeql-bundle-v${ver}" codeql-bundle-linux64.tar.gz
  tar -xzf "$DL/$name" -C "$dest" || die "codeql $ver extract failed"
  [[ -x "$dest/codeql/codeql" ]] || die "codeql $ver: codeql/codeql missing after extract"
  # pinned-version marks the bundle as shipped-with-the-toolcache so the Action
  # uses it authoritatively even when its default bundle bumps (no re-download).
  : > "$dest/pinned-version"
  : > "$STAGE/CodeQL/$ver/$ARCH.complete"
fi

# Read-only in the guest; make sure everything is traversable/readable.
chmod -R a+rX "$STAGE"

# ---- Pack the staged tree into a labelled ext4 -----------------------------
if [[ -z "$SIZE_MB" ]]; then
  used_mb="$(du -sm "$STAGE" | awk '{print $1}')"
  SIZE_MB=$(( used_mb + used_mb * MARGIN_PCT / 100 + 64 ))
fi
log "sizing drive at ${SIZE_MB}MB"

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
mkfs.ext4 -q -F -d "$STAGE" "$OUT" "${SIZE_MB}M"
# firerunner mounts by label, so this MUST be `hostedtoolcache` (16-byte cap).
e2label "$OUT" "$LABEL"
sync

log "tool-cache drive written: $OUT ($(du -h "$OUT" | awk '{print $1}'), label=$LABEL)"
log "contents:"
for t in node go Python CodeQL; do
  [[ -d "$STAGE/$t" ]] && for d in "$STAGE/$t"/*/; do echo "   - $t $(basename "$d")"; done
done
log "next: attach with  firerunner --toolcache $OUT  (or FR_TOOLCACHE=$OUT)"
