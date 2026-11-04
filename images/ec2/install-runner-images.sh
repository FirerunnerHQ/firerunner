# shellcheck shell=bash
# install-runner-images.sh — provisions the "full" toolset: the installers
# actions/runner-images runs for ubuntu-24.04, in the order its Packer template
# lists them. Reading the order from the template keeps this in step with a
# new release: only RI_REF changes. build-ec2-ami.sh prepends this to
# provision.sh, which calls install_runner_images on the build instance, as
# root.
#
# Left out, by name, in RI_SKIP:
#   - what only the Azure image build needs, or that would fight this image's
#     own boot (cleanup and validation, configure-system, the apt mock);
#   - the Android SDK, the largest single install, and Homebrew, which refuses
#     to install as root, until a job needs them.
#
# Settings: RI_REF, the runner-images tag; RI_SKIP, a space-separated list.
# curate_toolset FILE trims GitHub's toolset to what an image pays for. Older
# versions come back through setup-* actions; the defaults stay, because tools
# such as /usr/bin/go link to them.
curate_toolset() {
	local tmp
	tmp="$(mktemp)"
	jq '
		def last_only: if (.versions | length) > 0 then .versions |= [.[-1]] else . end;
		.toolcache |= map(select(.name != "CodeQL" and .name != "PyPy")) |
		.toolcache |= map(
			if .name == "Python" then .versions |= .[-2:]
			elif .name == "node" or .name == "Ruby" then last_only
			elif .name == "go" then .versions = ([.versions[-1], .default] | map(select(.)) | unique)
			else . end) |
		.java.versions = [.java.default] |
		.clang |= (last_only | .default_version = .versions[-1]) |
		.gcc |= last_only | .gfortran |= last_only | .dotnet |= last_only |
		.azureModules = [] |
		.node_modules |= map(select(.name == "typescript" or .name == "yarn"))
	' "$1" >"$tmp"
	mv "$tmp" "$1"
}

install_runner_images() {
	local ig=/imagegeneration tgz src s failed="" began used
	tgz="$(mktemp)"
	curl -fsSL -o "$tgz" "https://github.com/actions/runner-images/archive/refs/tags/${RI_REF}.tar.gz"
	src="$(mktemp -d)"
	tar xzf "$tgz" -C "$src" --strip-components=1
	rm -f "$tgz"

	install -d "$ig/helpers" "$ig/installers"
	cp -a "$src/images/ubuntu/scripts/helpers/." "$ig/helpers/"
	cp -a "$src/images/ubuntu/scripts/build/." "$ig/installers/"
	cp -a "$src/images/ubuntu/scripts/tests" "$ig/tests"
	# "netcat" is a virtual package here, which apt cannot install by that name.
	sed 's/"netcat"/"netcat-openbsd"/' "$src/images/ubuntu/toolsets/toolset-2404.json" >"$ig/installers/toolset.json"
	if [ "$TOOLSET" = standard ]; then
		curate_toolset "$ig/installers/toolset.json"
		# install-docker.sh pulls images that a job can pull when it needs them.
		sed -i 's/^\( *\)docker pull /\1: docker pull /' "$ig/installers/install-docker.sh"
	fi
	# Upstream's self-tests take hours and need Pester; the image is tested
	# end to end instead.
	printf '#!/bin/sh\n' >"$ig/helpers/invoke-tests.sh"
	sed -i '/Invoke-PesterTests/d' "$ig"/installers/*.ps1

	export HELPER_SCRIPTS="$ig/helpers" HELPER_SCRIPT_FOLDER="$ig/helpers" \
		INSTALLER_SCRIPT_FOLDER="$ig/installers" IMAGE_FOLDER="$ig" \
		IMAGE_OS=ubuntu24 IMAGE_VERSION="${RI_REF#ubuntu24/}" \
		IMAGEDATA_FILE="$ig/imagedata.json" DEBIAN_FRONTEND=noninteractive \
		ACCEPT_EULA=Y NEEDRESTART_MODE=a HOME=/root SUDO_USER=root \
		AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
	# configure-environment.sh edits the Azure agent's config.
	touch /etc/waagent.conf

	# Some scripts call apt-get without -y, which Azure's base image allows.
	printf 'APT::Get::Assume-Yes "true";\n' >/etc/apt/apt.conf.d/10-firerunner-assume-yes

	# The template's scripts, in order. A failed optional installer fails the
	# build: an image that silently lacks a tool is what this replaces.
	# shellcheck disable=SC2013 # script names hold no spaces
	for s in $(grep -o 'scripts/build/[A-Za-z0-9._-]*' \
		"$src/images/ubuntu/templates/build.ubuntu-24_04.pkr.hcl" | sed 's|.*/||'); do
		case " $RI_SKIP " in *" $s "*) continue ;; esac
		mark "runner-images $s"
		began=$SECONDS
		used="$(df --output=used -k / | tail -1)"
		case "$s" in
		*.ps1) pwsh -NoLogo -NoProfile -File "$ig/installers/$s" ;;
		*) bash -e "$ig/installers/$s" ;;
		esac || failed="$failed $s"
		[ -z "$failed" ] || { mark "runner-images FAILED:$failed"; return 1; }
		# What each tool costs, for deciding what the image should keep.
		mark "cost $s seconds=$((SECONDS - began)) mib=$((($(df --output=used -k / | tail -1) - used) / 1024))"
	done
	du -xsm /opt/* /opt/hostedtoolcache/* /usr/local/* /usr/share/* /usr/lib/* /home/* /etc/skel/.[a-z]* /var/lib/* 2>/dev/null |
		sort -rn | head -40 | while read -r mib path; do mark "du $path mib=$mib"; done || true
	rm -rf "$src" /etc/apt/apt.conf.d/10-firerunner-assume-yes /etc/waagent.conf
	apt-get -o DPkg::Lock::Timeout=600 clean
	# A job needs none of the build's scripts.
	rm -rf "$ig/installers" "$ig/helpers" "$ig/tests" /usr/local/bin/invoke_tests
}
