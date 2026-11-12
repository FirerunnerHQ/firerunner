package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
)

// A tier whose run panics stops every tier it is served with, as a deleted
// scale set does, and serveTiers returns the panic as an error: for a tenant
// its loop starts it again, and nothing reaches the daemon.
func TestServeTiersRecoversAPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := []func(context.Context) error{
		func(context.Context) error { panic("nil map") },
		func(c context.Context) error { <-c.Done(); return c.Err() },
	}
	err := serveTiers(ctx, cancel, []string{"a", "b"}, runs, slog.New(slog.DiscardHandler))
	if !errors.Is(err, errPanicked) || !strings.Contains(err.Error(), `tier "a"`) || !strings.Contains(err.Error(), "nil map") {
		t.Fatalf("serveTiers = %v, want the panic as an error", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the other tiers were left running")
	}
}

// A panic in a tenant's serve ends that try as an error: its stop still
// runs, the panic is counted, and the tenant starts again by itself. So does
// one in its start.
func TestTenantLoopRecoversAPanic(t *testing.T) {
	for _, where := range []string{"serve", "start"} {
		t.Run(where, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			live := &liveTenants{}
			var tries, stops atomic.Int32
			again := make(chan struct{})
			l := &tenantLoop{
				id: "acme", live: live, log: slog.New(slog.DiscardHandler), min: time.Millisecond, max: time.Millisecond,
				start: func(context.Context) (*servedTenant, error) {
					if tries.Add(1) == 1 && where == "start" {
						panic("start broke")
					}
					return &servedTenant{t: tenant{ID: "acme"}, bound: scheduler.NewTenant("acme", 0)}, nil
				},
				serve: func(ctx context.Context, _ context.CancelFunc, _ *servedTenant) error {
					if tries.Load() == 1 {
						panic("serve broke")
					}
					close(again)
					<-ctx.Done()
					return nil
				},
				stop: func(context.Context, *servedTenant) { stops.Add(1) },
			}
			done := make(chan struct{})
			go func() { defer close(done); l.run(ctx) }()
			<-again // it started again by itself
			h := live.tenants(nil, nil)
			cancel()
			<-done
			if want := map[string]int32{"serve": 2, "start": 1}[where]; stops.Load() != want {
				t.Fatalf("stopped %d time(s), want %d: a try that started stops, panic or not", stops.Load(), want)
			}
			var r *health.Restarts
			for _, ht := range h {
				if ht.ID == "acme" {
					r = ht.Restarts
				}
			}
			if r == nil || r.Count != 1 || r.Panics != 1 || !strings.Contains(r.LastError, where+" broke") {
				t.Fatalf("restarts = %+v, want 1 panic", r)
			}
		})
	}
}

// A tenant between tries is listed with its restarts, and forgotten with
// them once it is removed.
func TestLiveTenantsListAWaitingTenant(t *testing.T) {
	live := &liveTenants{}
	live.wait("acme", errors.New("start: 502"), time.Minute)
	h := live.tenants(nil, nil)
	if len(h) != 1 || h[0].ID != "acme" || h[0].NotServed == "" || h[0].Restarts == nil || h[0].Restarts.Count != 1 || h[0].Restarts.Panics != 0 {
		t.Fatalf("tenants = %+v", h)
	}
	live.forget("acme")
	if h := live.tenants(nil, nil); len(h) != 0 {
		t.Fatalf("after forget: %+v", h)
	}
	// Listed again later, it counts from none.
	live.wait("acme", errors.New("start: 502"), time.Minute)
	if h := live.tenants(nil, nil); len(h) != 1 || h[0].Restarts.Count != 1 {
		t.Fatalf("listed again: %+v", h)
	}
}
