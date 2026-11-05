# shellcheck shell=bash
# install-firerunner.sh — defines install_firerunner, which puts the binary for
# the git cache's proxy on the image. build-ec2-ami.sh prepends this to
# provision.sh, which runs it as a best-effort step.
#
# provision.sh calls it in an `||` list, where set -e does nothing, so every step
# checks itself: a binary that fails its checksum must never be installed. It
# carries job tokens. The binary must also have the subcommand: a release too
# old for it is the same as none.

FIRERUNNER_BIN="${FIRERUNNER_BIN:-/usr/local/bin/firerunner}"
FIRERUNNER_RELEASES="${FIRERUNNER_RELEASES:-https://github.com/FirerunnerHQ/firerunner/releases}"

install_firerunner() {
	local name dir
	name="firerunner_linux_$([ "$RUNNER_ARCH" = arm64 ] && echo arm64 || echo amd64)"
	dir="$(mktemp -d)" || return 1
	if ! fetch_firerunner "$name" "$dir" ||
		! chmod 0755 "$dir/$name" ||
		! "$dir/$name" git-proxy -h >/dev/null 2>&1 ||
		! install -m 0755 "$dir/$name" "$FIRERUNNER_BIN"; then
		rm -rf "$dir"
		return 1
	fi
	rm -rf "$dir"
}

# fetch_firerunner NAME DIR leaves a verified binary at DIR/NAME.
fetch_firerunner() {
	local name="$1" dir="$2" base
	if [ -n "${FR_URL:-}" ]; then
		curl -fsSL --retry 3 -o "$dir/$name" "$FR_URL" || return 1
		echo "$FR_SHA256  $dir/$name" | sha256sum -c - >/dev/null || return 1
		return 0
	fi
	base="$FIRERUNNER_RELEASES/latest/download"
	[ -z "${FR_VERSION:-}" ] || base="$FIRERUNNER_RELEASES/download/$FR_VERSION"
	curl -fsSL --retry 3 -o "$dir/$name" "$base/$name" || return 1
	curl -fsSL --retry 3 -o "$dir/SHA256SUMS" "$base/SHA256SUMS" || return 1
	(cd "$dir" && grep " $name\$" SHA256SUMS | sha256sum -c - >/dev/null) || return 1
}
