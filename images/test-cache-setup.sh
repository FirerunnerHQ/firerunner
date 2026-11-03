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
setup_nodocker() { FIRERUNNER_DOCKER=no-such-docker setup; }
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
line() { grep -n -- "$1" "$STATE/calls" | head -n 1 | cut -d: -f1; }
diverted() { grep -q 'results-receiver.actions.githubusercontent.com' "$R/etc/hosts"; }

# 1. A bundle on a host with Docker.
fresh docker control.fr.internal:8099
expect "docker: succeeds" "$(cat "$STATE/out" 2>/dev/null)" setup
expect "docker: the key is owner-only" "$(mode "$R/etc/firerunner-cache/server.key")" [ "$(mode "$R/etc/firerunner-cache/server.key")" = 600 ]
expect "docker: the key and leaf are the bundle's" "" bash -c "[ \"\$(cat '$R/etc/firerunner-cache/server.key')\" = KEY-PEM ] && [ \"\$(cat '$R/etc/firerunner-cache/server.pem')\" = LEAF-PEM ]"
expect "docker: the CA joins the system store" "$(cat "$STATE/calls")" \
	bash -c "[ \"\$(cat '$R/usr/local/share/ca-certificates/firerunner-cache.crt')\" = CA-PEM ] && grep -q update-ca-certificates '$STATE/calls'"
expect "docker: node gets the CA through externals" "" [ "$(cat "$STATE/runner/externals/firerunner-ca.pem")" = CA-PEM ]
expect "docker: nginx forwards the cache to the upstream" "" grep -q 'server control.fr.internal:8099;' "$R/etc/nginx/nginx.conf"
expect "docker: nginx checked, then started" "$(cat "$STATE/calls")" [ "$(line 'nginx -t')" -lt "$(line 'systemctl start nginx')" ]
expect "docker: containers resolve the results host to this VM" "$(cat "$STATE/calls")" \
	grep -q 'dnsmasq --listen-address=10.0.1.7 .*--address=/results-receiver.actions.githubusercontent.com/10.0.1.7' "$STATE/calls"
expect "docker: Docker hands containers that resolver, keeping its settings" "$(cat "$R/etc/docker/daemon.json")" \
	jq -e '.dns == ["10.0.1.7"] and .features["containerd-snapshotter"] == true' "$R/etc/docker/daemon.json"
expect "docker: and restarts with it" "" grep -q 'systemctl restart docker' "$STATE/calls"
expect "docker: the results host points here" "$(cat "$R/etc/hosts")" grep -qx '127.0.0.1 results-receiver.actions.githubusercontent.com' "$R/etc/hosts"

# 2. No Docker: no container resolver.
fresh nodocker 10.0.0.5:8099
expect "no docker: succeeds" "$(cat "$STATE/out" 2>/dev/null)" setup_nodocker
expect "no docker: no dnsmasq" "$(cat "$STATE/calls")" bash -c "! grep -q dnsmasq '$STATE/calls'"
expect "no docker: diverted" "" diverted

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
