# shellcheck shell=bash
# fetch-checked.sh — defines the download checks the EC2 build makes on the
# build instance. build-ec2-ami.sh prepends this to provision.sh, whose
# user-data cannot source images/lib/verify.sh; the builders there do the same
# checks on a host. Each function prints why it failed and returns non-zero.

# is_hex LENGTH STRING: STRING is LENGTH lowercase hex digits.
is_hex() {
	case "$2" in *[!0-9a-f]* | "") return 1 ;; esac
	[ "${#2}" = "$1" ]
}

# fetch_checked URL FILE SHA256 downloads URL to FILE, and fails, deleting
# FILE, unless FILE has that SHA-256.
fetch_checked() {
	is_hex 64 "$3" || {
		echo "fetch_checked: $1: no SHA-256 to check it against" >&2
		return 1
	}
	curl -fsSL --retry 3 -o "$2" "$1" || return 1
	if ! echo "$3  $2" | sha256sum -c - >/dev/null 2>&1; then
		echo "fetch_checked: $1: SHA-256 is not '$3'" >&2
		rm -f "$2"
		return 1
	fi
}

# fetch_commit URL COMMIT DIR checks out the git repository at URL at COMMIT, a
# full SHA, into the empty directory DIR. git hashes every object it
# receives, so the tree is the commit's, whatever a tag says now.
fetch_commit() {
	is_hex 40 "$2" || return 1
	git -C "$3" init -q &&
		git -C "$3" fetch -q --depth 1 "$1" "$2" &&
		git -C "$3" -c advice.detachedHead=false checkout -q FETCH_HEAD
}
