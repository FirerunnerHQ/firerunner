# shellcheck shell=bash
# verify.sh — checks what the image builders download before they use it.
# build-rootfs.sh, build-ubuntu-rootfs.sh, build-toolcache.sh, build-ec2-ami.sh
# and layer-ec2-ami.sh source it.
#
# Every archive a builder executes or bakes into an image is checked against a
# SHA-256 that is pinned, or read from a source other than the download itself:
#   actions/runner   the release notes ("<!-- BEGIN SHA linux-x64 -->…")
#   Node.js          SHASUMS256.txt in the same nodejs.org release directory,
#                    once its signature checks out against a release key in
#                    keys/nodejs-release-keys.asc
#   Go               the go.dev download index (?mode=json)
#   GitHub releases  the asset's digest in the GitHub API (python-versions,
#                    the CodeQL bundle)
#   firerunner       its release workflow's attestation (gh attestation verify)
# and a git repository is fetched at a commit SHA, with git, which hashes each
# object it receives. Each function prints an error saying what failed and returns non-zero; the
# callers stop on that.
#
# VERIFIED_LOG, when set, receives one "SHA  WHAT" line per verified download:
# the record of what an image was built from.

GITHUB_API="${GITHUB_API:-https://api.github.com}"
GITHUB_GIT="${GITHUB_GIT:-https://github.com}"

verify_error() {
	echo "error: $*" >&2
	return 1
}

sha256_of() {
	if command -v sha256sum >/dev/null; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi | awk '{print $1}'
}

record_verified() { # SHA WHAT
	echo ">> verified $2: $1"
	[[ -z "${VERIFIED_LOG:-}" ]] || printf '%s  %s\n' "$1" "$2" >>"$VERIFIED_LOG"
}

# check_sha256 FILE SHA256 WHAT fails unless FILE has that SHA-256.
check_sha256() {
	local want got
	want="$(printf '%s' "$2" | tr 'A-F' 'a-f')"
	[[ "$want" =~ ^[0-9a-f]{64}$ ]] || verify_error "$3: no SHA-256 to check it against (got '$2')" || return
	got="$(sha256_of "$1")"
	[[ "$got" == "$want" ]] ||
		verify_error "$3: SHA-256 mismatch: expected $want, downloaded $got; not using it" || return
	record_verified "$want" "$3"
}

# fetch_checked URL FILE SHA256 WHAT downloads URL to FILE and checks it. A
# file that fails the check is deleted.
fetch_checked() {
	[[ "$3" =~ ^[0-9a-fA-F]{64}$ ]] || verify_error "$4: no SHA-256 to check it against (got '$3')" || return
	if ! curl -fsSL --retry 3 -o "$2" "$1"; then
		rm -f "$2"
		verify_error "$4: download failed: $1"
		return
	fi
	check_sha256 "$2" "$3" "$4" || {
		rm -f "$2"
		return 1
	}
}

# runner_release VERSION PLATFORM prints "VERSION SHA256" for the actions/runner
# asset for PLATFORM (linux-x64, linux-arm64) of VERSION, or of the latest
# release when VERSION is empty. GitHub's release notes carry each asset's
# SHA-256 between "<!-- BEGIN SHA linux-x64 -->" markers.
runner_release() {
	local url json version sha
	url="$GITHUB_API/repos/actions/runner/releases/latest"
	[[ -z "$1" ]] || url="$GITHUB_API/repos/actions/runner/releases/tags/v$1"
	json="$(curl -fsSL --retry 3 "$url")" ||
		verify_error "could not read actions/runner release ${1:-latest} from $url" || return
	version="$(printf '%s\n' "$json" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n 1)"
	[[ -n "$version" ]] || verify_error "could not resolve actions/runner release ${1:-latest}" || return
	[[ -z "$1" || "$version" == "$1" ]] || verify_error "actions/runner release v$1 says it is v$version" || return
	sha="$(printf '%s\n' "$json" |
		sed -n "s/.*<!-- BEGIN SHA $2 -->\([0-9a-f]\{64\}\)<!-- END SHA $2 -->.*/\1/p" | head -n 1)"
	[[ -n "$sha" ]] ||
		verify_error "actions/runner v$version: its release notes carry no SHA-256 for $2; pass --runner-sha256" || return
	echo "$version $sha"
}

# firerunner_release REPO VERSION NAME prints "VERSION SHA256" for the release
# asset NAME (firerunner_linux_amd64) of REPO's VERSION, or of its latest
# release when VERSION is empty, once gh attestation verify shows that REPO's
# release workflow built it from that tag on a GitHub-hosted runner. The
# SHA256SUMS beside it comes from the same release, so it proves nothing by
# itself; deploy-control.sh checks the same way. Needs gh, logged in.
firerunner_release() {
	local repo="$1" version="$2" name="$3" dir sha
	command -v gh >/dev/null ||
		verify_error "checking firerunner's build provenance needs gh (gh attestation verify)" || return
	if [[ -z "$version" ]]; then
		version="$(gh api "repos/$repo/releases/latest" --jq .tag_name)" ||
			verify_error "could not resolve $repo's latest release" || return
	fi
	[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
		verify_error "$repo's release '$version' is not a version tag" || return
	dir="$(mktemp -d)" || verify_error "could not make a temporary directory" || return
	if ! curl -fsSL --retry 3 -o "$dir/$name" "$GITHUB_GIT/$repo/releases/download/$version/$name"; then
		rm -rf "$dir"
		verify_error "could not download $name of $repo $version" || return
	fi
	if ! gh attestation verify "$dir/$name" --repo "$repo" \
		--signer-workflow "$repo/.github/workflows/release.yml" \
		--source-ref "refs/tags/$version" --deny-self-hosted-runners >/dev/null 2>"$dir/err"; then
		sed '/^$/d; s/^/  /' "$dir/err" >&2
		rm -rf "$dir"
		verify_error "$name: no attestation that $repo's release workflow built it for $version" || return
	fi
	sha="$(sha256_of "$dir/$name")"
	rm -rf "$dir"
	echo "$version $sha"
}

# The Node.js release keys SHASUMS256.txt must be signed with (keys/README.md).
NODE_KEYRING="${NODE_KEYRING:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../keys" && pwd)/nodejs-release-keys.asc}"

# node_sha256 VERSION FILE prints FILE's SHA-256 from SHASUMS256.txt in Node
# VERSION's release directory, once its detached signature, SHASUMS256.txt.sig,
# checks out against a key in NODE_KEYRING. Needs gpg and gpgv.
node_sha256() {
	local url sums sha dir signer
	url="https://nodejs.org/dist/v$1/SHASUMS256.txt"
	command -v gpgv >/dev/null && command -v gpg >/dev/null ||
		verify_error "Node v$1: checking SHASUMS256.txt's signature needs gpg and gpgv (gnupg); install them, or pin the archive's SHA-256" || return
	[[ -r "$NODE_KEYRING" ]] || verify_error "Node v$1: no release keys at $NODE_KEYRING" || return
	dir="$(mktemp -d)" || verify_error "Node v$1: could not make a temporary directory" || return
	# gpgv reads only a binary keyring; the armored one is what the repo reviews.
	if ! curl -fsSL --retry 3 -o "$dir/SHASUMS256.txt" "$url" ||
		! curl -fsSL --retry 3 -o "$dir/SHASUMS256.txt.sig" "$url.sig"; then
		rm -rf "$dir"
		verify_error "Node v$1: could not read $url and its .sig" || return
	fi
	if ! GNUPGHOME="$dir" gpg --batch --quiet --dearmor <"$NODE_KEYRING" >"$dir/keyring.gpg" 2>"$dir/err" ||
		! GNUPGHOME="$dir" gpgv --status-fd 3 --keyring "$dir/keyring.gpg" \
			"$dir/SHASUMS256.txt.sig" "$dir/SHASUMS256.txt" 3>"$dir/status" 2>>"$dir/err"; then
		sed 's/^/   /' "$dir/err" >&2
		rm -rf "$dir"
		verify_error "Node v$1: $url is not signed by a Node.js release key; if a new releaser signed it, run images/keys/update-nodejs-keys.sh" || return
	fi
	# gpgv accepts a revoked key's signature; a revoked release key is not one.
	if grep -q '^\[GNUPG:\] REVKEYSIG ' "$dir/status"; then
		rm -rf "$dir"
		verify_error "Node v$1: $url is signed by a revoked release key" || return
	fi
	signer="$(awk '$2 == "VALIDSIG" { print $NF; exit }' "$dir/status")"
	echo ">> Node v$1 SHASUMS256.txt signed by release key $signer" >&2
	sums="$(cat "$dir/SHASUMS256.txt")"
	rm -rf "$dir"
	sha="$(printf '%s\n' "$sums" | awk -v f="$2" '$2 == f { print $1; exit }')"
	[[ -n "$sha" ]] || verify_error "Node v$1: $url lists no $2" || return
	echo "$sha"
}

# go_sha256 FILE prints FILE's SHA-256 from the go.dev download index, which is
# served apart from the archive (dl.google.com). Needs jq.
go_sha256() {
	local url sha
	url="https://go.dev/dl/?mode=json&include=all"
	sha="$(curl -fsSL --retry 3 "$url" | jq -r --arg f "$1" '[.[].files[] | select(.filename == $f) | .sha256] | first // empty')" ||
		verify_error "could not read the Go download index $url" || return
	[[ -n "$sha" ]] || verify_error "the Go download index $url lists no $1" || return
	echo "$sha"
}

# github_asset_sha256 REPO TAG NAME prints the SHA-256 that GitHub recorded when
# release TAG's asset NAME was uploaded, as the API reports it. Assets uploaded
# before mid-2025 have none. Needs jq.
github_asset_sha256() {
	local url digest
	url="$GITHUB_API/repos/$1/releases/tags/$2"
	digest="$(curl -fsSL --retry 3 "$url" | jq -r --arg n "$3" '[.assets[] | select(.name == $n) | .digest // empty] | first // empty')" ||
		verify_error "could not read release $2 of $1 from $url" || return
	[[ "$digest" == sha256:* ]] ||
		verify_error "$1 $2: GitHub records no SHA-256 for $3; pin it with --checksums" || return
	echo "${digest#sha256:}"
}

# resolve_commit REPO REF prints the commit SHA that REF (a tag or branch) names
# in github.com/REPO now. A full commit SHA is its own answer.
resolve_commit() {
	local sha
	if [[ "$2" =~ ^[0-9a-f]{40}$ ]]; then
		echo "$2"
		return
	fi
	sha="$(curl -fsSL --retry 3 -H 'Accept: application/vnd.github.sha' "$GITHUB_API/repos/$1/commits/$2")" ||
		verify_error "could not resolve $1 $2 to a commit" || return
	[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || verify_error "$1 $2 resolved to '$sha', not a commit SHA" || return
	echo "$sha"
}

# fetch_commit REPO COMMIT DIR checks out github.com/REPO at COMMIT into the
# empty directory DIR. git hashes every object it receives, and FETCH_HEAD is
# the object asked for by its full SHA, so the tree is the commit's, whatever a
# tag says now.
fetch_commit() {
	[[ "$2" =~ ^[0-9a-f]{40}$ ]] || verify_error "$1: '$2' is not a commit SHA" || return
	git -C "$3" init -q &&
		git -C "$3" fetch -q --depth 1 "$GITHUB_GIT/$1" "$2" &&
		git -C "$3" -c advice.detachedHead=false checkout -q FETCH_HEAD ||
		verify_error "could not fetch $1 at commit $2" || return
	record_verified "$2" "$1 (git commit)"
}
