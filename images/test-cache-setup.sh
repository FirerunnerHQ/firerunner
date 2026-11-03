#!/usr/bin/env bash
# Unit tests for assets/firerunner-cache-setup.sh. Needs bash and jq; no
# network. Fakes on PATH play update-ca-certificates, nginx, systemctl,
# dnsmasq, docker and hostname, and record what they were asked, in order,
# into $STATE/calls.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/assets/firerunner-cache-setup.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

for cmd in update-ca-certificates systemctl dnsmasq docker; do
	# shellcheck disable=SC2016 # expanded by the fake
	printf '#!/bin/sh\necho "%s $*" >> "$STATE/calls"\n' "$cmd" >"$TMP/bin/$cmd"
done
cat >"$TMP/bin/nginx" <<'FAKE'
#!/bin/sh
echo "nginx $*" >> "$STATE/calls"
[ ! -f "$STATE/nginx-bad" ]
FAKE
cat >"$TMP/bin/hostname" <<'FAKE'
#!/bin/sh
echo "10.0.1.7 172.17.0.1"
FAKE
chmod +x "$TMP/bin/"*

# jq is real (the image has it); keep its directory, not the rest of PATH.
JQ_DIR="$(dirname "$(command -v jq)")"
export JQ_DIR

fails=0
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then
		echo "ok   $name"
	else
		echo "FAIL $name: $detail"
		fails=$((fails + 1))
	fi
}

# fresh NAME UPSTREAM: a scratch root with the image's files and a bundle.
fresh() {
	export STATE="$TMP/state-$1"
	R="$STATE/root"
	mkdir -p "$R/etc/firerunner" "$R/etc/nginx" "$R/etc/docker" "$R/usr/local/share/ca-certificates" "$STATE/runner/externals"
	cp "$HERE/assets/firerunner-cache-nginx.conf" "$R/etc/firerunner/cache-nginx.conf"
	printf '127.0.0.1 localhost\n' >"$R/etc/hosts"
	echo '{"features":{"containerd-snapshotter":true}}' >"$R/etc/docker/daemon.json"
	jq -n --arg up "$2" '{ca: "CA-PEM", cert: "LEAF-PEM", key: "KEY-PEM", upstream: $up}' >"$STATE/bundle.json"
}
setup() {
	PATH="${FAKE_PATH:-$TMP/bin}:$JQ_DIR:/usr/bin:/bin" FIRERUNNER_CACHE_ROOT="$R" FIRERUNNER_RUNNER_DIR="$STATE/runner" \
		sh "$SCRIPT" "$STATE/bundle.json" >"$STATE/out" 2>&1
}
refused() { ! setup; }
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
line() { grep -n -- "$1" "$STATE/calls" | head -n 1 | cut -d: -f1; }
diverted() { grep -q 'results-receiver.actions.githubusercontent.com' "$R/etc/hosts"; }

# 1. A bundle: the host is diverted to a loopback proxy; containers are not.
fresh host control.fr.internal:8099
expect "host: succeeds" "$(cat "$STATE/out" 2>/dev/null)" setup
expect "host: the key is owner-only" "$(mode "$R/etc/firerunner-cache/server.key")" [ "$(mode "$R/etc/firerunner-cache/server.key")" = 600 ]
expect "host: the key and leaf are the bundle's" "" bash -c "[ \"\$(cat '$R/etc/firerunner-cache/server.key')\" = KEY-PEM ] && [ \"\$(cat '$R/etc/firerunner-cache/server.pem')\" = LEAF-PEM ]"
expect "host: the CA joins the system store" "$(cat "$STATE/calls")" \
	bash -c "[ \"\$(cat '$R/usr/local/share/ca-certificates/firerunner-cache.crt')\" = CA-PEM ] && grep -q update-ca-certificates '$STATE/calls'"
expect "host: node gets the CA through externals" "" [ "$(cat "$STATE/runner/externals/firerunner-ca.pem")" = CA-PEM ]
expect "host: nginx forwards the cache to the upstream" "" grep -q 'server control.fr.internal:8099;' "$R/etc/nginx/nginx.conf"
expect "host: nginx listens on loopback only" "$(grep listen "$R/etc/nginx/nginx.conf")" \
	bash -c "[ \"\$(grep -c '^ *listen ' '$R/etc/nginx/nginx.conf')\" = \"\$(grep -c '^ *listen 127.0.0.1:' '$R/etc/nginx/nginx.conf')\" ]"
expect "host: nginx checked, then started" "$(cat "$STATE/calls")" [ "$(line 'nginx -t')" -lt "$(line 'systemctl start nginx')" ]
expect "host: the results host points here" "$(cat "$R/etc/hosts")" grep -qx '127.0.0.1 results-receiver.actions.githubusercontent.com' "$R/etc/hosts"
expect "host: containers keep GitHub's cache (buildkit would refuse the CA)" "$(cat "$STATE/calls")" \
	bash -c "! grep -q -e dnsmasq -e 'restart docker' '$STATE/calls' && [ \"\$(cat '$R/etc/docker/daemon.json')\" = '{\"features\":{\"containerd-snapshotter\":true}}' ]"

# 3. Anything that fails leaves nothing diverted: the job keeps GitHub's cache.
fresh nginx-bad 10.0.0.5:8099
touch "$STATE/nginx-bad"
expect "nginx rejects its config: fails" "it succeeded" refused
expect "nginx rejects its config: not started" "$(cat "$STATE/calls")" bash -c "! grep -q 'systemctl start nginx' '$STATE/calls'"
expect "nginx rejects its config: nothing diverted" "$(cat "$R/etc/hosts")" bash -c "! grep -q results-receiver '$R/etc/hosts'"
for up in 'a;b:80' 'control.fr.internal' 'host:' '' 'x/y:1'; do
	fresh "upstream-${#up}" "$up"
	expect "upstream '$up': refused" "it succeeded" refused
	expect "upstream '$up': nginx untouched, nothing diverted" "$(cat "$STATE/calls" 2>/dev/null)" \
		bash -c "[ ! -s '$R/etc/nginx/nginx.conf' ] && ! grep -q results-receiver '$R/etc/hosts'"
done

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
