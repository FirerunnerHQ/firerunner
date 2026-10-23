package schedule

import (
	"context"
	"log/slog"
	"time"
)

// Target is what a schedule resizes for one tier.
type Target struct {
	// SetPool resizes the stopped pool; nil for a tier without one. It must
	// end surplus stopped instances before it returns.
	SetPool func(ctx context.Context, n int)
	// SetMin changes the hot runners kept with no job queued.
	SetMin func(ctx context.Context, n int)
	// Applied, when set, is told every change once made.
	Applied func(Sizes)
}

// Follow applies sizes(now) to the target at once and whenever they change,
// checking every tick, until ctx ends. Shrinking ends stopped pool
// instances before idle running ones: the pool is resized first.
func Follow(ctx context.Context, sizes func(time.Time) Sizes, t Target, tick time.Duration, now func() time.Time, log *slog.Logger) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var last *Sizes
	apply := func() {
		s := sizes(now())
		if last != nil && *last == s {
			return
		}
		from := "start"
		if last != nil {
			from = describe(*last)
		}
		log.Info("schedule: resizing", "from", from, "to", describe(s), "min", s.Min, "pool", s.Pool)
		if t.SetPool != nil {
			t.SetPool(ctx, s.Pool)
		}
		t.SetMin(ctx, s.Min)
		last = &s
		if t.Applied != nil {
			t.Applied(s)
		}
	}
	apply()
	tk := time.NewTicker(tick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			apply()
		}
	}
}

func describe(s Sizes) string {
	if s.Window == "" {
		return "base"
	}
	return s.Window
}
