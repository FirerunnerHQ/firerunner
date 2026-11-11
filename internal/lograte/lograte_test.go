package lograte

import (
	"testing"
	"time"
)

// One warning of a kind gets through per interval; the rest are counted and
// reported with the next one let through. Kinds are spaced independently.
func TestLimiterSpacesEachKind(t *testing.T) {
	start := time.Unix(1000, 0)
	now := start
	l := &Limiter{Now: func() time.Time { return now }}
	step := func(kind string, wantHeld uint64, wantOK bool) {
		t.Helper()
		held, ok := l.Allow(kind)
		if held != wantHeld || ok != wantOK {
			t.Fatalf("at +%v, Allow(%q) = %d, %v; want %d, %v", now.Sub(start), kind, held, ok, wantHeld, wantOK)
		}
	}
	step("a", 0, true)
	step("a", 0, false)
	step("b", 0, true) // another kind is not held back by a's warning
	now = now.Add(Every - time.Nanosecond)
	step("a", 0, false)
	now = now.Add(time.Nanosecond)
	step("a", 2, true) // the two held back since
	step("a", 0, false)
	now = now.Add(Every)
	step("a", 1, true)
	step("b", 0, true)
}

// Interval replaces the default spacing.
func TestLimiterInterval(t *testing.T) {
	now := time.Unix(0, 0)
	l := &Limiter{Interval: time.Second, Now: func() time.Time { return now }}
	if _, ok := l.Allow("a"); !ok {
		t.Fatal("first warning held back")
	}
	now = now.Add(time.Second)
	if _, ok := l.Allow("a"); !ok {
		t.Fatal("a warning an interval later held back")
	}
}
