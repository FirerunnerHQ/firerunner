// Package lograte spaces out repeated warnings, so an event that can happen
// thousands of times a minute, such as a refused connection, still reaches
// the operator without flooding the log.
package lograte

import (
	"sync"
	"time"
)

// Every is the spacing the servers use: at most one warning a minute per
// kind.
const Every = time.Minute

// Limiter lets one warning of each kind through per interval and counts the
// ones it holds back. Those are reported with the next warning let through,
// so the last few are never logged when none follows: keep a counter beside
// it for the totals. The zero value spaces warnings Every apart. It is safe
// for concurrent use.
type Limiter struct {
	// Interval overrides Every when positive.
	Interval time.Duration
	// Now overrides time.Now, for tests.
	Now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	held map[string]uint64
}

// Allow reports whether a warning of kind may be logged now and, when it
// may, how many of that kind were held back since the last one let through.
func (l *Limiter) Allow(kind string) (held uint64, ok bool) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	every := Every
	if l.Interval > 0 {
		every = l.Interval
	}
	t := now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last, l.held = map[string]time.Time{}, map[string]uint64{}
	}
	if last, seen := l.last[kind]; seen && t.Sub(last) < every {
		l.held[kind]++
		return 0, false
	}
	held = l.held[kind]
	l.last[kind] = t
	delete(l.held, kind)
	return held, true
}
