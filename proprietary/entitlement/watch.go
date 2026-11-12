package entitlement

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"
)

// DefaultInterval is how often the daemon reads the store: a cap billing
// writes reaches a running daemon within this long.
const DefaultInterval = 30 * time.Second

// readTimeout bounds one read, so a hung store delays the next read only.
const readTimeout = 10 * time.Second

// Watcher reads a store at once and every Interval, and calls OnRead after
// every read, failed or not: a record expires with the clock, not with a
// change, so the decisions are taken again each time. A read that fails
// keeps the last records; their valid-until decides how long they hold.
type Watcher struct {
	Store    Store
	Interval time.Duration
	Log      *slog.Logger
	OnRead   func()
	// tick replaces the interval's ticker in tests.
	tick <-chan time.Time
	now  func() time.Time

	mu        sync.Mutex
	records   Records
	known     bool
	lastRead  time.Time
	lastErr   string
	lastErrAt time.Time
}

// Status is how reading the store goes.
type Status struct {
	Store string
	// Known is false until a read has succeeded.
	Known       bool
	LastRead    time.Time
	LastError   string
	LastErrorAt time.Time
}

// Read reads the store once.
func (w *Watcher) Read(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	rs, err := w.Store.Load(ctx)
	now := w.Now()
	w.mu.Lock()
	if err != nil {
		w.lastErr, w.lastErrAt = err.Error(), now
	} else {
		w.records, w.known, w.lastRead = rs, true, now
	}
	w.mu.Unlock()
	if err != nil && w.Log != nil {
		w.Log.Warn("read entitlements; keeping the last until they expire", "store", w.Store.String(), "err", err)
	}
	if w.OnRead != nil {
		w.OnRead()
	}
	return err
}

// Run reads the store every Interval until ctx ends. It does not read at
// once: call Read first, before serving.
func (w *Watcher) Run(ctx context.Context) {
	tick := w.tick
	if tick == nil {
		every := w.Interval
		if every <= 0 {
			every = DefaultInterval
		}
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			_ = w.Read(ctx)
		}
	}
}

// Records returns the latest records.
func (w *Watcher) Records() Records {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.records)
}

// Decide judges tenant id's latest record now.
func (w *Watcher) Decide(id string) (Record, bool, Decision) {
	w.mu.Lock()
	r, ok := w.records[id]
	w.mu.Unlock()
	return r, ok, Decide(r, ok, w.Now())
}

// Status returns how reading the store goes.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Status{Store: w.Store.String(), Known: w.known, LastRead: w.lastRead, LastError: w.lastErr, LastErrorAt: w.lastErrAt}
}

// Now is the watcher's clock.
func (w *Watcher) Now() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}
