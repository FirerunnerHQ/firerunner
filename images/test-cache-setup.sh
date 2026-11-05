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
# The instance's /bin/sh is dash; macOS's sh is bash.
POSIX_SH="$(command -v dash || echo sh)"
setup() {
	PATH="${FAKE_PATH:-$TMP/bin}:$JQ_DIR:/usr/bin:/bin" FIRERUNNER_CACHE_ROOT="$R" FIRERUNNER_RUNNER_DIR="$STATE/runner" \
		"$POSIX_SH" "$SCRIPT" "$STATE/bundle.json" >"$STATE/out" 2>&1
}
refused() { ! setup; }
# git_bundle: add the git proxy's identity to this scratch root's bundle.
git_bundle() {
	jq '. + {git: {ca: "GIT-CA", cert: "GIT-LEAF", key: "GIT-KEY"}}' "$STATE/bundle.json" >"$STATE/b" && mv "$STATE/b" "$STATE/bundle.json"
}
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

# 4. A tier with a git cache. The job runs git as its own user, in a directory
#    it must be able to reach, and the CA stays out of the system store.
fresh git control.fr.internal:8099
mkdir -p "$R/etc/ssl"
git_bundle
setup
expect "git: setup succeeds" "$(cat "$STATE/out")" [ "$?" = 0 ]
expect "git: the proxy starts, after nginx" "$(cat "$STATE/calls")" \
	[ "$(line 'systemctl start nginx')" -lt "$(line 'systemctl start firerunner-git-proxy.service')" ]
expect "git: its leaf and key are the bundle's" "" bash -c "[ \"\$(cat '$R/etc/firerunner-cache/git.pem')\" = GIT-LEAF ] && [ \"\$(cat '$R/etc/firerunner-cache/git.key')\" = GIT-KEY ]"
expect "git: its key is private" "$(mode "$R/etc/firerunner-cache/git.key")" [ "$(mode "$R/etc/firerunner-cache/git.key")" = 600 ]
expect "git: its CA is the bundle's" "" [ "$(cat "$R/etc/ssl/firerunner-git-ca.pem")" = GIT-CA ]
expect "git: the job's user can read its CA" "$(mode "$R/etc/ssl/firerunner-git-ca.pem")" [ "$(mode "$R/etc/ssl/firerunner-git-ca.pem")" = 644 ]
expect "git: its CA stays out of the system store" "" bash -c "! grep -rq GIT-CA '$R/usr/local/share/ca-certificates'"
expect "git: the results host still points here" "" grep -qx '127.0.0.1 results-receiver.actions.githubusercontent.com' "$R/etc/hosts"

fresh no-git 10.0.0.5:8099
setup
expect "no git cache: the proxy stays off" "$(cat "$STATE/calls")" bash -c "! grep -q firerunner-git-proxy '$STATE/calls'"

# A proxy that will not start costs the git cache, not actions/cache.
fresh git-fails 10.0.0.5:8099
mkdir -p "$R/etc/ssl"
git_bundle
mkdir -p "$STATE/failbin"
cat >"$STATE/failbin/systemctl" <<'FAKE'
#!/bin/sh
echo "systemctl $*" >> "$STATE/calls"
case "$*" in *firerunner-git-proxy*) exit 1 ;; esac
FAKE
chmod +x "$STATE/failbin/systemctl"
FAKE_PATH="$STATE/failbin:$TMP/bin" setup
expect "git proxy fails: setup still succeeds" "$(cat "$STATE/out")" [ "$?" = 0 ]
expect "git proxy fails: says so" "$(cat "$STATE/out")" grep -q 'no git cache' "$STATE/out"
expect "git proxy fails: actions/cache is diverted" "" grep -q results-receiver "$R/etc/hosts"

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
