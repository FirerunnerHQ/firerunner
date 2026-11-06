#!/usr/bin/env bash
# Unit tests for assets/firerunner-git-config.sh, with real git and a fake curl.
# Each test reads back what git itself sees.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/assets/firerunner-git-config.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
# The instance's /bin/sh is dash; macOS's sh is bash.
POSIX_SH="$(command -v dash || echo sh)"
export GIT_CONFIG_SYSTEM="$TMP/gitconfig" GIT_CONFIG_GLOBAL=/dev/null

cat >"$TMP/bin/curl" <<'FAKE'
#!/bin/sh
n="$(cat "$TMP/count" 2>/dev/null || echo 0)"
n=$((n + 1))
echo "$n" >"$TMP/count"
[ "$n" -gt "${CURL_FAILS:-0}" ]
FAKE
chmod +x "$TMP/bin/curl"
export TMP

fails=0
pass() { # DESC CMD...
	local d="$1"
	shift
	if "$@"; then echo "ok   $d"; else
		echo "FAIL $d"
		fails=$((fails + 1))
	fi
}
run() {
	rm -f "$TMP/count"
	rc=0
	PATH="$TMP/bin:$PATH" FIRERUNNER_GIT_PROXY_TRIES="${TRIES:-5}" "$POSIX_SH" "$SCRIPT" "$@" 2>"$TMP/stderr" || rc=$?
}
get() { git config --system --get-urlmatch "$1" "$2" 2>/dev/null || true; }

: >"$GIT_CONFIG_SYSTEM"
run on
pass "on: exits 0" [ "$rc" = 0 ]
pass "on: github.com goes through the proxy" [ "$(get http.proxy https://github.com/o/r.git)" = http://127.0.0.1:3129 ]
pass "on: names the CA for github.com" [ "$(get http.sslCAInfo https://github.com/o/r.git)" = /etc/ssl/firerunner-git-ca.pem ]
pass "on: other hosts are untouched" [ -z "$(get http.proxy https://gitlab.com/o/r.git)" ]
pass "on: other hosts keep the system's CAs" [ -z "$(get http.sslCAInfo https://gitlab.com/o/r.git)" ]
run on
pass "on twice: exits 0" [ "$rc" = 0 ]
pass "on twice: one value" [ "$(git config --system --get-all http.https://github.com/.proxy | wc -l | tr -d ' ')" = 1 ]

run off
pass "off: exits 0" [ "$rc" = 0 ]
pass "off: github.com goes direct" [ -z "$(get http.proxy https://github.com/o/r.git)" ]
pass "off: the CA is gone too" [ -z "$(get http.sslCAInfo https://github.com/o/r.git)" ]
run off
pass "off twice: exits 0" [ "$rc" = 0 ]

# The CA goes first, as the proxy alone is a fetch that trusts nothing. A fake
# git on PATH records the order, then does the work.
mkdir -p "$TMP/order"
cat >"$TMP/order/git" <<FAKE
#!/bin/sh
echo "\$*" >>"$TMP/calls"
exec "$(command -v git)" "\$@"
FAKE
chmod +x "$TMP/order/git"
: >"$TMP/calls"
PATH="$TMP/order:$PATH" run on
pass "on: the CA is written before the proxy" [ "$(grep -n sslCAInfo "$TMP/calls" | head -1 | cut -d: -f1)" -lt "$(grep -n '\.proxy' "$TMP/calls" | head -1 | cut -d: -f1)" ]
run off

# A lock another git left must not keep git pointed at a dead port.
run on
: >"$GIT_CONFIG_SYSTEM.lock"
began=$SECONDS
run off
took=$((SECONDS - began))
pass "stale lock: exits 0" [ "$rc" = 0 ]
pass "stale lock: github.com goes direct" [ -z "$(get http.proxy https://github.com/o/r.git)" ]
# systemd kills this past the unit's budget, which would leave the lock and the
# proxy in place, so recovery has to finish well inside it.
budget="$(sed -n 's/^TimeoutStopSec=//p' "$HERE/assets/firerunner-git-proxy.service")"
pass "stale lock: recovers inside the unit's stop budget ($took s of $budget s)" [ "$took" -lt "$budget" ]

# The image's git fails at once on a lock, where newer git waits a second of its
# own. A lock another git holds for a moment must be left alone: fake git fails
# like the image's, and fake sleep is that other git finishing.
mkdir -p "$TMP/live"
REAL_GIT="$(command -v git)"
REAL_RM="$(command -v rm)"
cat >"$TMP/live/git" <<FAKE
#!/bin/sh
case "\$*" in
*--remove-section*) [ ! -e "$GIT_CONFIG_SYSTEM.lock" ] || exit 255 ;;
esac
exec "$REAL_GIT" "\$@"
FAKE
cat >"$TMP/live/sleep" <<FAKE
#!/bin/sh
"$REAL_RM" -f "$GIT_CONFIG_SYSTEM.lock"
FAKE
cat >"$TMP/live/rm" <<FAKE
#!/bin/sh
case "\$*" in *.lock*) echo "\$*" >>"$TMP/removed" ;; esac
exec "$REAL_RM" "\$@"
FAKE
chmod +x "$TMP/live/git" "$TMP/live/sleep" "$TMP/live/rm"
run on
: >"$GIT_CONFIG_SYSTEM.lock"
rm -f "$TMP/removed"
PATH="$TMP/live:$PATH" run off
pass "live lock: exits 0" [ "$rc" = 0 ]
pass "live lock: github.com goes direct" [ -z "$(get http.proxy https://github.com/o/r.git)" ]
pass "live lock: it was left for its owner" [ ! -e "$TMP/removed" ]

# A file it cannot change at all must be reported, not assumed.
run on
mkdir "$TMP/ro" && cp "$GIT_CONFIG_SYSTEM" "$TMP/ro/gitconfig"
chmod 0555 "$TMP/ro"
GIT_CONFIG_SYSTEM="$TMP/ro/gitconfig" run off
pass "cannot remove it: reported" [ "$rc" != 0 ]
chmod 0755 "$TMP/ro"
run off

# A proxy still starting: wait for it, as the first fetch must not fail.
CURL_FAILS=3 run on
pass "slow proxy: waits, then sets" [ "$rc" = 0 ]
pass "slow proxy: is set" [ -n "$(get http.proxy https://github.com/o/r.git)" ]
pass "slow proxy: asked until it answered" [ "$(cat "$TMP/count")" = 4 ]
run off

# A proxy that never answers must leave git going direct.
CURL_FAILS=99 run on
pass "dead proxy: fails, so systemd restarts it" [ "$rc" != 0 ]
pass "dead proxy: git goes direct" [ -z "$(get http.proxy https://github.com/o/r.git)" ]
pass "dead proxy: says so" grep -q 'never answered' "$TMP/stderr"

run
pass "no argument: refused" [ "$rc" = 2 ]

if [ "$fails" -ne 0 ]; then
	echo "$fails failure(s)"
	exit 1
fi
echo "all passed"
