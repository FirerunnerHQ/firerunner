#!/bin/sh
#
# firerunner-git-config on|off — point git at this instance's git proxy, or stop.
# firerunner-git-proxy.service runs it, so git uses the proxy only while it is
# up: git treats a dead proxy as a failed fetch, not as a reason to go direct.
#
# Only git, and only for github.com: anything else on the instance is untouched,
# and the CA is named here rather than added to the system store.
set -eu

PROXY="${FIRERUNNER_GIT_PROXY:-127.0.0.1:3129}"
CA="${FIRERUNNER_GIT_CA:-/etc/ssl/firerunner-git-ca.pem}"
TRIES="${FIRERUNNER_GIT_PROXY_TRIES:-50}"
CONFIG="${GIT_CONFIG_SYSTEM:-/etc/gitconfig}"
SECTION="http.https://github.com/"

case "${1:-}" in
on)
	# A proxy that is not listening yet would fail the first fetch.
	tries=0
	until curl -s -o /dev/null -m 1 "http://$PROXY/"; do
		tries=$((tries + 1))
		if [ "$tries" -ge "$TRIES" ]; then
			echo "firerunner-git-config: the proxy never answered" >&2
			exit 1
		fi
		sleep 0.1
	done
	# The CA first: with it alone, a fetch still goes direct and works, where
	# the proxy alone would be a fetch that trusts nothing.
	git config --system "$SECTION.sslCAInfo" "$CA"
	git config --system "$SECTION.proxy" "http://$PROXY"
	;;
off)
	# This must work: a config left behind sends every fetch to a dead port for
	# the rest of the job. The usual obstacle is a lock another git left.
	tries=0
	while git config --system "$SECTION.proxy" >/dev/null 2>&1; do
		git config --system --remove-section "$SECTION" 2>/dev/null && exit 0
		tries=$((tries + 1))
		if [ "$tries" -ge "$TRIES" ]; then
			rm -f "$CONFIG.lock"
		fi
		if [ "$tries" -gt "$TRIES" ]; then
			echo "firerunner-git-config: could not remove the proxy from $CONFIG" >&2
			exit 1
		fi
		sleep 0.1
	done
	# The CA alone is harmless, but leaves nothing behind either.
	git config --system --remove-section "$SECTION" 2>/dev/null || true
	;;
*)
	echo "usage: firerunner-git-config on|off" >&2
	exit 2
	;;
esac
