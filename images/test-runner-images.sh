#!/usr/bin/env bash
# Unit tests for ec2/install-runner-images.sh's curate_toolset, on a toolset
# shaped like actions/runner-images'. Needs jq.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# shellcheck source=ec2/install-runner-images.sh disable=SC1091
. "$HERE/ec2/install-runner-images.sh"
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
# or the latest, into a versioned directory, and proved by importing it.
bin="$TMP/bin"
mkdir -p "$bin"
cat >"$bin/curl" <<'STUB'
#!/bin/sh
for a; do last="$a"; done
echo "$last" >>"$CALLS"
case "$last" in
*/Pester/5.9.0) printf 'https://cdn.example/packages/pester.5.9.0.nupkg' ;;
*/PSScriptAnalyzer) printf 'https://cdn.example/packages/psscriptanalyzer.1.25.0.nupkg' ;;
*) exit 22 ;;
esac
STUB
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

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
