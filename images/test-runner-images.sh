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
    {"name": "go", "versions": ["1.24.*", "1.25.*", "1.26.*"], "default": "1.24.*"},
    {"name": "Ruby", "versions": ["3.2.*", "4.0.*"]},
    {"name": "CodeQL", "versions": ["*"]}
  ],
  "java": {"default": "17", "versions": ["8", "11", "17", "21"]},
  "clang": {"versions": ["16", "17", "18"], "default_version": "17"},
  "gcc": {"versions": ["g++-12", "g++-14"]},
  "gfortran": {"versions": ["gfortran-12", "gfortran-14"]},
  "dotnet": {"versions": ["8.0", "10.0"]},
  "azureModules": [{"name": "az"}],
  "node_modules": [{"name": "grunt"}, {"name": "typescript"}, {"name": "yarn"}],
  "apt": {"common_packages": ["netcat-openbsd"]}
}
JSON
curate_toolset "$TMP/toolset.json"

tool() { echo ".toolcache[] | select(.name == \"$1\")"; }
expect "go keeps its default, which /usr/bin/go links to" "$(tool go).versions | index(\"1.24.*\") != null"
expect "go keeps its latest" "$(tool go).versions | index(\"1.26.*\") != null"
expect "go drops the rest" "($(tool go).versions | length) == 2"
expect "go's default stays" "($(tool go).default) == \"1.24.*\""
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
expect "global npm modules shrink" '[.node_modules[].name] == ["typescript", "yarn"]'
expect "what it does not know stays" '.apt.common_packages == ["netcat-openbsd"]'

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
