// Package schedule sizes a tier by the clock: in a window (some days, some
// hours, in one time zone) the tier keeps a given number of hot runners and
// stopped pool instances; outside every window it keeps its base sizes.
package schedule

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	// Time zones resolve on hosts without a zoneinfo database.
	_ "time/tzdata"
)

// Schedule is a tier's "schedule" block:
//
//	{"timezone": "Asia/Taipei",
//	 "windows": [{"days": "mon-fri", "hours": "06:00-13:00", "min": 2, "pool": 12}]}
type Schedule struct {
	TimeZone string   `json:"timezone"`
	Windows  []Window `json:"windows"`

	loc *time.Location
}

// Window is one sized period. Days is "daily", or a comma list of days and
// day ranges ("mon-fri", "sat,sun"). Hours is "HH:MM-HH:MM"; an end at or
// before the start runs past midnight, and the window belongs to the day it
// starts on. The first window that covers a moment sizes it.
type Window struct {
	Days  string `json:"days"`
	Hours string `json:"hours"`
	Min   int    `json:"min"`
	Pool  int    `json:"pool"`

	days       [7]bool
	start, end int // minutes after midnight
}

// Sizes are what a tier keeps at a moment.
type Sizes struct {
	Min  int
	Pool int
	// Window names the covering window ("mon-fri 06:00-13:00"), or is empty
	// for the base sizes.
	Window string
}

// UnmarshalJSON parses and validates the block, so a bad schedule fails at
// config load.
func (s *Schedule) UnmarshalJSON(b []byte) error {
	type plain Schedule
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*s = Schedule(p)
	return s.init()
}

func (s *Schedule) init() error {
	if s.TimeZone == "" {
		return fmt.Errorf("schedule: timezone is required (e.g. \"UTC\", \"Asia/Taipei\")")
	}
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return fmt.Errorf("schedule: timezone %q: %w", s.TimeZone, err)
	}
	s.loc = loc
	if len(s.Windows) == 0 {
		return fmt.Errorf("schedule: at least one window is required")
	}
	for i := range s.Windows {
		w := &s.Windows[i]
		if err := w.init(); err != nil {
			return fmt.Errorf("schedule: window %d: %w", i, err)
		}
	}
	return nil
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func (w *Window) init() error {
	if w.Min < 0 || w.Pool < 0 {
		return fmt.Errorf("min and pool must be >= 0")
	}
	days := strings.ToLower(strings.TrimSpace(w.Days))
	switch days {
	case "":
		return fmt.Errorf("days is required (\"daily\", \"mon-fri\", \"sat,sun\")")
	case "daily", "*":
		for d := range w.days {
			w.days[d] = true
		}
	default:
		for _, part := range strings.Split(days, ",") {
			from, to, isRange := strings.Cut(strings.TrimSpace(part), "-")
			a, ok := dayNames[from]
			if !ok {
				return fmt.Errorf("days %q: unknown day %q (use sun..sat)", w.Days, from)
			}
			b := a
			if isRange {
				if b, ok = dayNames[to]; !ok {
					return fmt.Errorf("days %q: unknown day %q (use sun..sat)", w.Days, to)
				}
			}
			// A range may wrap the week: "fri-mon".
			for d := a; ; d = (d + 1) % 7 {
				w.days[d] = true
				if d == b {
					break
				}
			}
		}
	}
	from, to, ok := strings.Cut(w.Hours, "-")
	if !ok {
		return fmt.Errorf("hours %q must be HH:MM-HH:MM", w.Hours)
	}
	var err error
	if w.start, err = clock(from, false); err != nil {
		return fmt.Errorf("hours %q: %w", w.Hours, err)
	}
	if w.end, err = clock(to, true); err != nil {
		return fmt.Errorf("hours %q: %w", w.Hours, err)
	}
	return nil
}

// clock parses HH:MM into minutes after midnight; 24:00 only as an end.
func clock(s string, end bool) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || len(h) != 2 || len(m) != 2 || mm < 0 || mm > 59 || hh < 0 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	if hh > 23 && !(end && hh == 24 && mm == 0) {
		return 0, fmt.Errorf("%q is past 23:59", s)
	}
	return hh*60 + mm, nil
}

// covers reports whether the window covers t (already in the schedule's zone).
func (w *Window) covers(t time.Time) bool {
	now := t.Hour()*60 + t.Minute()
	day := t.Weekday()
	if w.start < w.end {
		return w.days[day] && now >= w.start && now < w.end
	}
	// Past midnight: the evening part on a listed day, or the morning part
	// on the day after one.
	return (w.days[day] && now >= w.start) || (w.days[(day+6)%7] && now < w.end)
}

func (w *Window) name() string { return w.Days + " " + w.Hours }

// At returns the sizes for t: the first covering window's, or base.
func (s *Schedule) At(t time.Time, base Sizes) Sizes {
	local := t.In(s.loc)
	for i := range s.Windows {
		if w := &s.Windows[i]; w.covers(local) {
			return Sizes{Min: w.Min, Pool: w.Pool, Window: w.name()}
		}
	}
	base.Window = ""
	return base
}

// MaxPool is the largest pool any window asks for.
func (s *Schedule) MaxPool() int {
	n := 0
	for _, w := range s.Windows {
		n = max(n, w.Pool)
	}
	return n
}

// MaxMin is the largest min any window asks for.
func (s *Schedule) MaxMin() int {
	n := 0
	for _, w := range s.Windows {
		n = max(n, w.Min)
	}
	return n
}
