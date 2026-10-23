package schedule

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, js string) *Schedule {
	t.Helper()
	var s Schedule
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatalf("parse %s: %v", js, err)
	}
	return &s
}

// The issue's example: weekdays 06-13 and 20-23 in Taipei, pool 12 hot 2;
// otherwise the base, pool 2 hot 0.
const example = `{"timezone": "Asia/Taipei", "windows": [
	{"days": "mon-fri", "hours": "06:00-13:00", "min": 2, "pool": 12},
	{"days": "mon-fri", "hours": "20:00-23:00", "min": 2, "pool": 12}]}`

var base = Sizes{Min: 0, Pool: 2}

func taipei(t *testing.T, s string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Taipei")
	at, err := time.ParseInLocation("Mon 2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestExampleTransitions(t *testing.T) {
	s := parse(t, example)
	for _, c := range []struct {
		at        string
		min, pool int
		window    string
	}{
		{"Mon 2026-10-19 05:59", 0, 2, ""},
		{"Mon 2026-10-19 06:00", 2, 12, "mon-fri 06:00-13:00"}, // start is inclusive
		{"Mon 2026-10-19 12:59", 2, 12, "mon-fri 06:00-13:00"},
		{"Mon 2026-10-19 13:00", 0, 2, ""}, // end is exclusive
		{"Mon 2026-10-19 20:00", 2, 12, "mon-fri 20:00-23:00"},
		{"Mon 2026-10-19 23:00", 0, 2, ""},
		{"Fri 2026-10-23 22:59", 2, 12, "mon-fri 20:00-23:00"},
		{"Sat 2026-10-24 06:00", 0, 2, ""}, // weekend
		{"Sun 2026-10-25 21:00", 0, 2, ""},
	} {
		got := s.At(taipei(t, c.at), base)
		if got.Min != c.min || got.Pool != c.pool || got.Window != c.window {
			t.Errorf("%s: got %+v, want min %d pool %d window %q", c.at, got, c.min, c.pool, c.window)
		}
	}
}

func TestTimeZoneIsTheSchedules(t *testing.T) {
	// 22:30 UTC Sunday is 06:30 Monday in Taipei: inside the window.
	s := parse(t, example)
	if got := s.At(time.Date(2026, 10, 18, 22, 30, 0, 0, time.UTC), base); got.Pool != 12 {
		t.Fatalf("got %+v, want the Monday-morning window", got)
	}
}

func TestPastMidnightBelongsToTheStartDay(t *testing.T) {
	s := parse(t, `{"timezone": "UTC", "windows": [{"days": "fri", "hours": "22:00-02:00", "min": 1, "pool": 4}]}`)
	at := func(d, hm string) Sizes {
		tm, err := time.Parse("2006-01-02 15:04", d+" "+hm)
		if err != nil {
			t.Fatal(err)
		}
		return s.At(tm, base)
	}
	if at("2026-10-23", "23:00").Pool != 4 { // Friday evening
		t.Error("Friday 23:00 not covered")
	}
	if at("2026-10-24", "01:59").Pool != 4 { // Saturday small hours, Friday's window
		t.Error("Saturday 01:59 not covered by Friday's window")
	}
	if at("2026-10-24", "02:00").Pool != 2 {
		t.Error("Saturday 02:00 still covered")
	}
	if at("2026-10-23", "01:00").Pool != 2 { // Thursday is not listed
		t.Error("Friday 01:00 covered though Thursday is not listed")
	}
}

func TestFirstWindowWins(t *testing.T) {
	s := parse(t, `{"timezone": "UTC", "windows": [
		{"days": "daily", "hours": "09:00-10:00", "min": 3, "pool": 1},
		{"days": "daily", "hours": "00:00-24:00", "min": 1, "pool": 5}]}`)
	nine := time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)
	if got := s.At(nine, base); got.Min != 3 || got.Pool != 1 {
		t.Fatalf("got %+v, want the first window", got)
	}
	if got := s.At(nine.Add(time.Hour), base); got.Min != 1 || got.Pool != 5 {
		t.Fatalf("got %+v, want the all-day window", got)
	}
	if s.MaxPool() != 5 || s.MaxMin() != 3 {
		t.Fatalf("MaxPool %d MaxMin %d", s.MaxPool(), s.MaxMin())
	}
}

func TestDayLists(t *testing.T) {
	s := parse(t, `{"timezone": "UTC", "windows": [{"days": "sat,sun", "hours": "00:00-24:00", "min": 0, "pool": 7},
		{"days": "Fri-Mon", "hours": "12:00-13:00", "min": 0, "pool": 9}]}`)
	noon := func(day int) int { return s.At(time.Date(2026, 10, day, 12, 30, 0, 0, time.UTC), base).Pool }
	// 2026-10-23 is a Friday.
	for day, want := range map[int]int{22: 2, 23: 9, 24: 7, 25: 7, 26: 9, 27: 2} {
		if got := noon(day); got != want {
			t.Errorf("Oct %d noon: pool %d, want %d", day, got, want)
		}
	}
}

func TestInvalid(t *testing.T) {
	for name, js := range map[string]string{
		"no timezone":  `{"windows": [{"days": "daily", "hours": "00:00-01:00", "min": 1}]}`,
		"bad timezone": `{"timezone": "Mars/Olympus", "windows": [{"days": "daily", "hours": "00:00-01:00", "min": 1}]}`,
		"no windows":   `{"timezone": "UTC", "windows": []}`,
		"no days":      `{"timezone": "UTC", "windows": [{"hours": "00:00-01:00", "min": 1}]}`,
		"bad day":      `{"timezone": "UTC", "windows": [{"days": "monday", "hours": "00:00-01:00", "min": 1}]}`,
		"bad range":    `{"timezone": "UTC", "windows": [{"days": "mon-xyz", "hours": "00:00-01:00", "min": 1}]}`,
		"no dash":      `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "06:00", "min": 1}]}`,
		"one digit":    `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "6:00-13:00", "min": 1}]}`,
		"25 o'clock":   `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "06:00-25:00", "min": 1}]}`,
		"24 as start":  `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "24:00-01:00", "min": 1}]}`,
		"bad minute":   `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "06:60-07:00", "min": 1}]}`,
		"negative":     `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "06:00-07:00", "pool": -1}]}`,
		"plus sign":    `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "+6:00-07:00", "min": 1}]}`,
		"sizes none":   `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "06:00-07:00"}]}`,
		"unknown key":  `{"timezone": "UTC", "windows": [{"days": "daily", "hours": "06:00-07:00", "hot": 2}]}`,
	} {
		var s Schedule
		err := json.Unmarshal([]byte(js), &s)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "schedule") && !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: error %q does not say it is the schedule", name, err)
		}
	}
}

func TestOmittedSizeKeepsTheBase(t *testing.T) {
	s := parse(t, `{"timezone": "UTC", "windows": [{"days": "sat,sun", "hours": "00:00-24:00", "min": 1}]}`)
	sat := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if got := s.At(sat, Sizes{Min: 0, Pool: 4}); got.Min != 1 || got.Pool != 4 {
		t.Fatalf("got %+v, want min from the window and the base pool", got)
	}
	if s.MaxPool() != 0 {
		t.Fatalf("MaxPool %d for windows that set no pool", s.MaxPool())
	}
}
