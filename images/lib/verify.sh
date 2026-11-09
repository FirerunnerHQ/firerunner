# shellcheck shell=bash
# verify.sh — checks what the image builders download before they use it.
# build-rootfs.sh and build-ubuntu-rootfs.sh source it.
#
# Every archive a builder executes or bakes into an image is checked against a
# SHA-256 that is pinned, or read from a source other than the download itself:
#   actions/runner   the release notes ("<!-- BEGIN SHA linux-x64 -->…")
#   Node.js          SHASUMS256.txt in the same nodejs.org release directory
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

# node_sha256 VERSION FILE prints FILE's SHA-256 from SHASUMS256.txt in Node
# VERSION's release directory.
node_sha256() {
	local url sums sha
	url="https://nodejs.org/dist/v$1/SHASUMS256.txt"
	sums="$(curl -fsSL --retry 3 "$url")" || verify_error "Node v$1: could not read $url" || return
	sha="$(printf '%s\n' "$sums" | awk -v f="$2" '$2 == f { print $1; exit }')"
	[[ -n "$sha" ]] || verify_error "Node v$1: $url lists no $2" || return
	echo "$sha"
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
