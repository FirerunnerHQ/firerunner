#!/usr/bin/env bash
# Unit tests for ec2/install-runner-images.sh's curate_toolset, on a toolset
# shaped like actions/runner-images', and for the download checks it and
# provision.sh make (ec2/fetch-checked.sh). Needs jq, git and openssl.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# shellcheck source=ec2/install-runner-images.sh disable=SC1091
. "$HERE/ec2/install-runner-images.sh"
# shellcheck source=ec2/fetch-checked.sh disable=SC1091
. "$HERE/ec2/fetch-checked.sh"
mark() { echo "mark $*" >>"$CALLS"; }
fails=0
expect() { # DESC JQ-EXPRESSION
	if jq -e "$2" "$TMP/toolset.json" >/dev/null; then echo "ok   $1"; else
		echo "FAIL $1"
		fails=$((fails + 1))
	fi
}

cat >"$TMP/toolset.json" <<'JSON'
{
  "toolcache": [
    {"name": "Python", "versions": ["3.11.*", "3.12.*", "3.13.*", "3.14.*"]},
    {"name": "PyPy", "versions": ["3.9"]},
    {"name": "node", "versions": ["22.*", "24.*"]},
    {"name": "go", "versions": ["1.24.*", "1.25.*", "1.26.*"], "default": "1.25.*"},
    {"name": "Ruby", "versions": ["3.2.*", "4.0.*"]},
    {"name": "CodeQL", "versions": ["*"]}
  ],
  "java": {"default": "17", "versions": ["8", "11", "17", "21"]},
  "clang": {"versions": ["16", "17", "18"], "default_version": "17"},
  "gcc": {"versions": ["g++-12", "g++-14"]},
  "gfortran": {"versions": ["gfortran-12", "gfortran-14"]},
  "dotnet": {"versions": ["8.0", "10.0"]},
  "azureModules": [{"name": "az"}],
  "powershellModules": [{"name": "Microsoft.Graph"}, {"name": "Pester", "versions": ["5.9.0"]}],
  "node_modules": [{"name": "grunt"}, {"name": "typescript"}, {"name": "yarn"}],
  "apt": {"common_packages": ["netcat-openbsd"]}
}
JSON
curate_toolset "$TMP/toolset.json"

tool() { echo ".toolcache[] | select(.name == \"$1\")"; }
expect "go keeps its default, which /usr/bin/go links to" "$(tool go).versions | index(\"1.25.*\") != null"
expect "go keeps its latest" "$(tool go).versions | index(\"1.26.*\") != null"
expect "go drops the rest" "($(tool go).versions | length) == 2"
expect "go's default stays" "($(tool go).default) == \"1.25.*\""
expect "Python keeps two" "$(tool Python).versions == [\"3.13.*\", \"3.14.*\"]"
expect "node keeps the latest" "$(tool node).versions == [\"24.*\"]"
expect "Ruby keeps the latest" "$(tool Ruby).versions == [\"4.0.*\"]"
expect "CodeQL and PyPy go" '[.toolcache[].name] | (index("CodeQL") == null and index("PyPy") == null)'
expect "Java keeps its default" '.java.versions == ["17"]'
expect "clang's default follows the version it keeps" '.clang == {"versions": ["18"], "default_version": "18"}'
expect "gcc keeps the latest" '.gcc.versions == ["g++-14"]'
expect "gfortran keeps the latest" '.gfortran.versions == ["gfortran-14"]'
expect ".NET keeps the latest" '.dotnet.versions == ["10.0"]'
expect "Azure PowerShell modules go" '.azureModules == []'
expect "Microsoft.Graph goes, and the other modules stay" '[.powershellModules[].name] == ["Pester"]'
expect "global npm modules shrink" '[.node_modules[].name] == ["typescript", "yarn"]'
expect "what it does not know stays" '.apt.common_packages == ["netcat-openbsd"]'

# install_powershell_modules: from the package URL, at the toolset's version
# or the latest, checked against the gallery's SHA-512, into a versioned
# directory, and proved by importing it. The fake curl serves packages from
# $FIX, and the gallery's hash of FIX/NAME.real.
bin="$TMP/bin"
export FIX="$TMP/fix"
mkdir -p "$bin" "$FIX"
cat >"$bin/curl" <<'STUB'
#!/bin/sh
out=""
while [ $# -gt 1 ]; do
	[ "$1" != -o ] || out="$2"
	shift
done
echo "$1" >>"$CALLS"
case "$1" in
*"Packages(Id='"*) # the gallery's entry for a package
	id="${1#*Id=\'}"
	id="${id%%\'*}"
	printf '<entry><m:properties><d:PackageHash>%s</d:PackageHash><d:PackageHashAlgorithm>SHA512</d:PackageHashAlgorithm></m:properties></entry>\n' \
		"$(openssl dgst -sha512 -binary "$FIX/$id.real" | base64 | tr -d '\n')"
	;;
*/Pester/5.9.0) cp "$FIX/Pester" "$out" && printf 'https://cdn.example/packages/pester.5.9.0.nupkg' ;;
*/PSScriptAnalyzer) cp "$FIX/PSScriptAnalyzer" "$out" && printf 'https://cdn.example/packages/psscriptanalyzer.1.25.0.nupkg' ;;
*) exit 22 ;;
esac
STUB
if ! command -v sha512sum >/dev/null; then
	printf '#!/bin/sh\nexec shasum -a 512 "$@"\n' >"$bin/sha512sum"
	chmod +x "$bin/sha512sum"
fi
for m in Pester PSScriptAnalyzer; do
	echo "$m package" >"$FIX/$m"
	cp "$FIX/$m" "$FIX/$m.real"
done
cat >"$bin/unzip" <<'STUB'
#!/bin/sh
for a; do last="$a"; done
echo "unzip $last" >>"$CALLS"
STUB
cat >"$bin/pwsh" <<'STUB'
#!/bin/sh
echo "pwsh $*" >>"$CALLS"
STUB
chmod +x "$bin/curl" "$bin/unzip" "$bin/pwsh"
export CALLS="$TMP/calls"
: >"$CALLS"
cat >"$TMP/modules.json" <<'JSON'
{"powershellModules": [{"name": "Pester", "versions": ["5.9.0"]}, {"name": "PSScriptAnalyzer"}]}
JSON
PATH="$bin:$PATH" PS_MODULE_DIR="$TMP/mods" install_powershell_modules "$TMP/modules.json"
has() {
	if grep -qF -- "$2" "$CALLS"; then echo "ok: $1"; else echo "FAIL: $1"; fails=$((fails + 1)); fi
}
has "a pinned module's version" "https://www.powershellgallery.com/api/v2/package/Pester/5.9.0"
has "an unpinned module takes the latest" "https://www.powershellgallery.com/api/v2/package/PSScriptAnalyzer"
has "the pinned module's directory" "unzip $TMP/mods/Pester/5.9.0"
has "the version comes from the package's name" "unzip $TMP/mods/PSScriptAnalyzer/1.25.0"
has "the module is imported" "Import-Module '$TMP/mods/PSScriptAnalyzer/1.25.0/PSScriptAnalyzer.psd1'"
has "the gallery's hash is asked for the version it served" "Packages(Id='PSScriptAnalyzer',Version='1.25.0')"
lacks() {
	if grep -qF -- "$2" "$CALLS"; then echo "FAIL: $1"; fails=$((fails + 1)); else echo "ok: $1"; fi
}

# A package that is not what the gallery lists stops the build there.
: >"$CALLS"
echo "a backdoor" >"$FIX/PSScriptAnalyzer"
rc=0
PATH="$bin:$PATH" PS_MODULE_DIR="$TMP/mods2" install_powershell_modules "$TMP/modules.json" || rc=$?
pass() { if [ "$2" = 0 ]; then echo "ok: $1"; else echo "FAIL: $1"; fails=$((fails + 1)); fi; }
pass "a tampered module fails the step" "$([ "$rc" != 0 ] && echo 0 || echo 1)"
has "a tampered module says why" "mark FAILED PowerShell module PSScriptAnalyzer 1.25.0"
lacks "a tampered module is never unpacked" "unzip $TMP/mods2/PSScriptAnalyzer"
lacks "a tampered module is never imported" "PSScriptAnalyzer.psd1"
has "the modules before it still install" "unzip $TMP/mods2/Pester/5.9.0"

# fetch_checked, which provision.sh downloads the runner with.
printf 'runner\n' >"$FIX/runner.tgz"
cat >"$bin/curl" <<'STUB'
#!/bin/sh
while [ $# -gt 1 ]; do
	[ "$1" != -o ] || out="$2"
	shift
done
cp "$FIX/served" "$out"
STUB
want="$(openssl dgst -sha256 -r "$FIX/runner.tgz" | cut -d' ' -f1)"
if ! command -v sha256sum >/dev/null; then
	printf '#!/bin/sh\nexec shasum -a 256 "$@"\n' >"$bin/sha256sum"
	chmod +x "$bin/sha256sum"
fi
cp "$FIX/runner.tgz" "$FIX/served"
rc=0
PATH="$bin:$PATH" fetch_checked https://x/runner.tgz "$TMP/got.tgz" "$want" 2>/dev/null || rc=$?
pass "fetch_checked: the right file passes" "$rc"
echo "a backdoor" >"$FIX/served"
rc=0
PATH="$bin:$PATH" fetch_checked https://x/runner.tgz "$TMP/bad.tgz" "$want" 2>/dev/null || rc=$?
pass "fetch_checked: a tampered file fails" "$([ "$rc" != 0 ] && echo 0 || echo 1)"
pass "fetch_checked: a tampered file is deleted" "$([ ! -e "$TMP/bad.tgz" ] && echo 0 || echo 1)"
rc=0
PATH="$bin:$PATH" fetch_checked https://x/runner.tgz "$TMP/none.tgz" "" 2>/dev/null || rc=$?
pass "fetch_checked: no checksum fails" "$([ "$rc" != 0 ] && echo 0 || echo 1)"
cp "$FIX/runner.tgz" "$FIX/served"
rc=0
PATH="$bin:$PATH" fetch_checked https://x/runner.tgz "$TMP/short.tgz" "${want%?}" 2>/dev/null || rc=$?
pass "fetch_checked: a truncated checksum fails" "$([ "$rc" != 0 ] && echo 0 || echo 1)"

# fetch_runner_images: the commit, by real git from a local repository, and
# never a tag or branch name.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
repo="$TMP/runner-images"
mkdir -p "$repo"
git -C "$repo" init -q
echo A >"$repo/which"
git -C "$repo" add which
git -C "$repo" -c user.name=t -c user.email=t@example.com commit -q -m A
first="$(git -C "$repo" rev-parse HEAD)"
echo B >"$repo/which"
git -C "$repo" -c user.name=t -c user.email=t@example.com commit -q -am B
export RI_GIT="file://$repo" RI_REF=ubuntu24/test
: >"$CALLS"
mkdir "$TMP/ri1"
rc=0
RI_COMMIT="$first" fetch_runner_images "$TMP/ri1" 2>/dev/null || rc=$?
pass "runner-images: fetches the pinned commit" "$rc"
pass "runner-images: the tree is that commit's" "$([ "$(cat "$TMP/ri1/which")" = A ] && echo 0 || echo 1)"
for bad in "$(printf '%040d' 1)" "$(git -C "$repo" branch --show-current)" "${first%?}" ""; do
	d="$(mktemp -d "$TMP/ri.XXXXXX")"
	rc=0
	RI_COMMIT="$bad" fetch_runner_images "$d" 2>/dev/null || rc=$?
	pass "runner-images: '$bad' fails" "$([ "$rc" != 0 ] && echo 0 || echo 1)"
	pass "runner-images: '$bad' leaves no tree" "$([ ! -e "$d/which" ] && echo 0 || echo 1)"
done
has "runner-images: a failed fetch says so" "mark FAILED runner-images: could not fetch commit"

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
