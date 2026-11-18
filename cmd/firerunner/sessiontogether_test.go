package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeSet is a scale set whose session another daemon holds for its first
// heldFor tries.
type fakeSet struct {
	mu       sync.Mutex
	heldFor  int
	tries    int
	open     bool
	released int
	err      error
}

func (f *fakeSet) TryOpenSession(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	if f.err != nil {
		return false, f.err
	}
	if f.tries <= f.heldFor {
		return false, nil
	}
	f.open = true
	return true, nil
}

func (f *fakeSet) ReleaseSession(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open {
		f.open = false
		f.released++
	}
}

func sets(f ...*fakeSet) []sessionSet {
	out := make([]sessionSet, len(f))
	for i := range f {
		out[i] = f[i]
	}
	return out
}

// A tenant's sessions open all together, or none is held: one held by
// another daemon has those opened before it released at once, so that two
// daemons can never each hold part and wait on the other. Once all are free,
// all open; standby says so meanwhile.
func TestOpenTogether(t *testing.T) {
	a, b, c := &fakeSet{}, &fakeSet{heldFor: 2}, &fakeSet{}
	var standby []bool
	err := openTogether(context.Background(), sets(a, b, c), time.Millisecond, func(on bool) { standby = append(standby, on) }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !a.open || !b.open || !c.open {
		t.Fatalf("open: %v %v %v, want all", a.open, b.open, c.open)
	}
	if a.released != 2 || c.tries != 1 {
		t.Fatalf("a released %d times, c tried %d: want a released at each round b was held, c tried once all came free", a.released, c.tries)
	}
	if !slices.Equal(standby, []bool{true, true, false}) {
		t.Fatalf("standby %v, want on while held, then off", standby)
	}

	// All free at once: no standby, one try each.
	d, e := &fakeSet{}, &fakeSet{}
	standby = nil
	if err := openTogether(context.Background(), sets(d, e), time.Millisecond, func(on bool) { standby = append(standby, on) }, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if d.tries != 1 || e.tries != 1 || slices.Contains(standby, true) {
		t.Fatalf("tries %d %d, standby %v", d.tries, e.tries, standby)
	}
}

// It stops with its context, holding nothing; and an error other than a
// session held fails at once, releasing what it opened.
func TestOpenTogetherStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, b := &fakeSet{}, &fakeSet{heldFor: 1 << 30}
	done := make(chan error, 1)
	go func() { done <- openTogether(ctx, sets(a, b), time.Millisecond, nil, slog.New(slog.DiscardHandler)) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openTogether = %v, want the context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop with its context")
	}
	a.mu.Lock()
	open := a.open
	a.mu.Unlock()
	if open {
		t.Fatal("a session held after stopping")
	}

	boom := errors.New("401 Unauthorized")
	c, d := &fakeSet{}, &fakeSet{err: boom}
	if err := openTogether(context.Background(), sets(c, d), time.Millisecond, nil, slog.New(slog.DiscardHandler)); !errors.Is(err, boom) {
		t.Fatalf("openTogether = %v, want the error", err)
	}
	if c.open || c.released != 1 {
		t.Fatalf("c open %v, released %d: want it released", c.open, c.released)
	}
}
