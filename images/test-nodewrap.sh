#!/usr/bin/env bash
# Tests for assets/nodewrap.c: build it with cc, put it where the runner's
# node would be, and check what the real node (a fake) is run with. Linux
# only (nodewrap finds itself through /proc/self/exe).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ext="$TMP/externals"
mkdir -p "$ext/node20/bin"
# Static, as the AMI builds it: it must run in musl job containers too.
cc -O2 -static -o "$ext/node20/bin/node" "$HERE/assets/nodewrap.c"
if command -v file >/dev/null && ! file "$ext/node20/bin/node" | grep -q 'statically linked'; then
	echo "FAIL nodewrap is not statically linked: $(file "$ext/node20/bin/node")"
	exit 1
fi
# The fake node is python, not sh: dash (Ubuntu's sh) drops dashed names
# itself, which would hide what nodewrap passes on.
cat >"$ext/node20/bin/node.real" <<'FAKE'
#!/usr/bin/env python3
import os, sys
print("ca=" + os.environ.get("NODE_EXTRA_CA_CERTS", ""))
print("args=" + " ".join(sys.argv[1:]))
print("dashed=%d" % (os.environ.get("INPUT_INCLUDE-HIDDEN-FILES") == "yes"))
FAKE
chmod +x "$ext/node20/bin/node.real"

fails=0
expect() {
	local name="$1" got="$2" want="$3"
	if printf '%s\n' "$got" | grep -qx -- "$want"; then
		echo "ok   $name"
	else
		echo "FAIL $name: got $(printf '%s' "$got" | tr '\n' '|')"
		fails=$((fails + 1))
	fi
}
node() { env 'INPUT_INCLUDE-HIDDEN-FILES=yes' "$@" "$ext/node20/bin/node" script.js --flag; }

out="$(node)"
expect "no CA file: no extra CA (node would warn about a missing one)" "$out" 'ca='
expect "no CA file: the real node gets the arguments" "$out" 'args=script.js --flag'
expect "a dashed action input survives (a shell wrapper would drop it)" "$out" 'dashed=1'

echo CA >"$ext/firerunner-ca.pem"
out="$(node)"
expect "CA file: node gets it" "$out" "ca=$ext/node20/bin/../../firerunner-ca.pem"
out="$(node NODE_EXTRA_CA_CERTS=/job/own.pem)"
expect "a job's own NODE_EXTRA_CA_CERTS is kept" "$out" 'ca=/job/own.pem'

rm "$ext/node20/bin/node.real"
rc=0
node >/dev/null 2>&1 || rc=$?
expect "no real node: exit 127" "$rc" 127

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
