package tenantstate

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"
)

// DefaultInterval is how often the daemon reads the store: a suspension
// reaches a running daemon within this long.
const DefaultInterval = 15 * time.Second

// readTimeout bounds one read, so a hung store delays the next read only.
const readTimeout = 10 * time.Second

// Watcher reads a store at once and every Interval, and tells OnChange the
// states after the first read and after each that changed them. A read that
// fails keeps the last states: a suspension is lifted only by a read that no
// longer holds it.
type Watcher struct {
	Store    Store
	Interval time.Duration
	Log      *slog.Logger
	OnChange func(States)
	// tick replaces the interval's ticker in tests.
	tick <-chan time.Time
	now  func() time.Time

	mu        sync.Mutex
	states    States
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
	st, err := w.Store.Load(ctx)
	now := w.clock()
	w.mu.Lock()
	if err != nil {
		w.lastErr, w.lastErrAt = err.Error(), now
		w.mu.Unlock()
		if w.Log != nil {
			w.Log.Warn("read tenant states; keeping the last", "store", w.Store.String(), "err", err)
		}
		return err
	}
	changed := !w.known || !maps.Equal(st, w.states)
	w.states, w.known, w.lastRead = st, true, now
	w.mu.Unlock()
	if changed && w.OnChange != nil {
		w.OnChange(maps.Clone(st))
	}
	return nil
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

// States returns the latest states, and whether any read has succeeded.
func (w *Watcher) States() (States, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.states), w.known
}

// Status returns how reading the store goes.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Status{Store: w.Store.String(), Known: w.known, LastRead: w.lastRead, LastError: w.lastErr, LastErrorAt: w.lastErrAt}
}

func (w *Watcher) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}
