#!/usr/bin/env bash
# Tests for mutate.sh against a throwaway Go module in a temp dir: one guard,
# one test that pins it, and one function no test looks at. Checks the
# verdict line, the exit status and that the file is restored byte-for-byte.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MUTATE="$HERE/mutate.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export GOFLAGS='' GOWORK=off GOTOOLCHAIN=local

fails=0
expect() {
	local name="$1" detail="$2"
	shift 2
	if "$@"; then echo "ok   $name"; else echo "FAIL $name: $detail"; fails=$((fails + 1)); fi
}

# fresh <name>: a new module in $MOD. guard.go deliberately ends without a
# newline, so a restore that adds or drops one is caught by cmp.
fresh() {
	MOD="$TMP/$1"
	mkdir -p "$MOD/guard"
	printf 'module example.com/m\n\ngo 1.21\n' >"$MOD/go.mod"
	printf '%s\n' 'package guard' '' \
		'// Allow admits n only above the limit.' \
		'func Allow(n int) bool { return n > 10 }' '' \
		'// Unchecked is covered by no test.' \
		'func Unchecked() int { return 1 }' '' >"$MOD/guard/guard.go"
	printf '// end' >>"$MOD/guard/guard.go"
	printf '%s\n' 'package guard' '' 'import "testing"' '' \
		'func TestAllow(t *testing.T) {' \
		'	if !Allow(11) || Allow(10) {' \
		'		t.Fatal("limit is wrong")' \
		'	}' '}' >"$MOD/guard/guard_test.go"
	cp "$MOD/guard/guard.go" "$TMP/$1.pristine"
	FILE="$MOD/guard/guard.go"
	PRISTINE="$TMP/$1.pristine"
}
# mutate <args...>: run mutate.sh from the module root; output in $OUT,
# the last line in $LAST, exit status in $RC.
mutate() {
	OUT="$TMP/out"
	(cd "$MOD" && "$MUTATE" "$@") >"$OUT" 2>&1 && RC=0 || RC=$?
	LAST="$(tail -n 1 "$OUT")"
}
restored() { cmp -s "$PRISTINE" "$FILE"; }
verdict_is() { [ "${LAST%% *}" = "$1" ]; }
no_verdict() { ! grep -Eq "^(CAUGHT|SURVIVED)" "$OUT"; }

# 1. A mutant the test catches.
fresh caught
mutate guard/guard.go 'n > 10' 'n >= 10'
expect "caught: verdict" "$(cat "$OUT")" verdict_is CAUGHT
expect "caught: exit 0" "rc=$RC" [ "$RC" = 0 ]
expect "caught: shows the diff" "" grep -q '^+func Allow(n int) bool { return n >= 10 }' "$OUT"
expect "caught: restored" "" restored

# 2. A mutant no test notices.
fresh survived
mutate guard/guard.go 'return 1' 'return 2'
expect "survived: verdict" "$(cat "$OUT")" verdict_is SURVIVED
expect "survived: exit 1" "rc=$RC" [ "$RC" = 1 ]
expect "survived: restored" "" restored

# 3. A mutant that does not compile is never reported as caught.
fresh invalid
mutate guard/guard.go 'return n > 10' 'return n >'
expect "no compile: verdict" "$(cat "$OUT")" verdict_is INVALID
expect "no compile: says so" "$LAST" grep -q 'does not compile' <<<"$LAST"
expect "no compile: exit 2" "rc=$RC" [ "$RC" = 2 ]
expect "no compile: restored" "" restored

# 4. Old text that is missing, or not unique, changes nothing.
fresh notfound
mutate guard/guard.go 'n < 10' 'n <= 10'
expect "not found: verdict" "$(cat "$OUT")" verdict_is INVALID
expect "not found: says so" "$LAST" grep -q 'not found' <<<"$LAST"
expect "not found: exit 2" "rc=$RC" [ "$RC" = 2 ]
expect "not found: untouched" "" restored
mutate guard/guard.go 'func ' 'func  '
expect "ambiguous: verdict" "$(cat "$OUT")" verdict_is INVALID
expect "ambiguous: says so" "$LAST" grep -q 'more than once' <<<"$LAST"
expect "ambiguous: untouched" "" restored
mutate guard/guard.go '' 'x'
expect "empty old text: INVALID" "$(cat "$OUT")" verdict_is INVALID

# 5. Tests already failing make any verdict meaningless.
fresh baseline
sed 's/Allow(10)/Allow(11)/' "$MOD/guard/guard_test.go" >"$TMP/t" && mv "$TMP/t" "$MOD/guard/guard_test.go"
mutate guard/guard.go 'n > 10' 'n >= 10'
expect "failing baseline: INVALID" "$(cat "$OUT")" verdict_is INVALID
expect "failing baseline: says so" "$LAST" grep -q 'fail before mutating' <<<"$LAST"
expect "failing baseline: untouched" "" restored

# 6. Custom go test arguments after --; a hang is caught by the timeout.
fresh custom
mutate guard/guard.go 'n > 10' 'n >= 10' -- -run TestAllow ./guard
expect "custom args: CAUGHT" "$(cat "$OUT")" verdict_is CAUGHT
mutate guard/guard.go 'n > 10' 'n >= 10' -- -run TestNothing ./guard
expect "custom args: no test ran, SURVIVED" "$(cat "$OUT")" verdict_is SURVIVED
fresh hang
MUTATE_TIMEOUT=2s mutate guard/guard.go 'return n > 10' 'for {}'
expect "hang: CAUGHT by the timeout" "$(cat "$OUT")" verdict_is CAUGHT
expect "hang: restored" "" restored

# 7. Interrupted mid-run: a go wrapper sends mutate.sh SIGTERM while the
# mutant is in place; the file still comes back.
fresh signal
mkdir -p "$TMP/bin"
real_go="$(command -v go)"
cat >"$TMP/bin/go" <<FAKE
#!/usr/bin/env bash
if grep -q 'n >= 10' "$FILE"; then kill -TERM "\$PPID"; fi
exec "$real_go" "\$@"
FAKE
chmod +x "$TMP/bin/go"
PATH="$TMP/bin:$PATH" MUTATE_BASELINE=0 mutate guard/guard.go 'n > 10' 'n >= 10' -- ./guard
expect "SIGTERM: exit 143" "rc=$RC $(cat "$OUT")" [ "$RC" = 143 ]
expect "SIGTERM: no verdict" "$LAST" no_verdict
expect "SIGTERM: restored" "" restored

# 8. Usage errors.
fresh usage
mutate guard/guard.go 'n > 10'
expect "too few args: exit 2" "rc=$RC" [ "$RC" = 2 ]
mutate guard/guard.go 'n > 10' 'n >= 10' -run X
expect "args without --: exit 2" "rc=$RC" [ "$RC" = 2 ]
mutate nope.go 'a' 'b'
expect "missing file: INVALID" "$(cat "$OUT")" verdict_is INVALID
expect "usage error: untouched" "" restored

if [ "$fails" -gt 0 ]; then
	echo "$fails test(s) failed"
	exit 1
fi
echo "all mutate.sh tests passed"
