package schedule

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// recorder logs the target's calls in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) target(pool bool) Target {
	t := Target{
		SetMin:  func(_ context.Context, n int) { r.add(fmt.Sprintf("min=%d", n)) },
		Applied: func(s Sizes) { r.add("applied " + describe(s)) },
	}
	if pool {
		t.SetPool = func(_ context.Context, n int) { r.add(fmt.Sprintf("pool=%d", n)) }
	}
	return t
}

func TestFollowAppliesTransitionsPoolFirst(t *testing.T) {
	s := parse(t, example)
	// A clock that steps through a Monday: before, in, and after the
	// morning window, with a repeat to show unchanged sizes do nothing.
	steps := []string{"Mon 2026-10-19 05:59", "Mon 2026-10-19 06:00", "Mon 2026-10-19 06:01", "Mon 2026-10-19 13:00"}
	var mu sync.Mutex
	i := 0
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at := taipei(t, steps[min(i, len(steps)-1)])
		i++
		return at
	}
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Follow(ctx, func(at time.Time) Sizes { return s.At(at, base) }, r.target(true), time.Millisecond, now, nil)
		close(done)
	}()
	want := []string{
		"pool=2", "min=0", "applied base", // at start
		"pool=12", "min=2", "applied mon-fri 06:00-13:00",
		"pool=2", "min=0", "applied base", // shrink: the pool before the hot runners
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(r.snapshot()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the clock now repeats 13:00: no more calls
	cancel()
	<-done
	got := r.snapshot()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
}

func TestFollowWithoutAPool(t *testing.T) {
	s := parse(t, example)
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // apply once at start, then return
	Follow(ctx, func(at time.Time) Sizes { return s.At(at, base) }, r.target(false), time.Hour,
		func() time.Time { return taipei(t, "Tue 2026-10-20 07:00") }, nil)
	if got := fmt.Sprint(r.snapshot()); got != "[min=2 applied mon-fri 06:00-13:00]" {
		t.Fatalf("calls %s", got)
	}
}
