package entitlement

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	now     = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	valid   = now.Add(time.Hour)
	expired = now.Add(-time.Second)
)

// Every state, valid and expired, and a missing or unreadable record: what
// launches, what is held, and why.
func TestDecide(t *testing.T) {
	for name, tc := range map[string]struct {
		r          Record
		ok         bool
		hold, open bool
		warn       bool
		why        string
	}{
		"no record":                 {ok: false, hold: true, why: "no entitlement record"},
		"unreadable record":         {r: Record{Invalid: "bad json"}, ok: true, hold: true, why: "does not parse: bad json"},
		"active ok":                 {r: Record{Account: Active, Budget: OK, ValidUntil: valid}, ok: true},
		"active warning":            {r: Record{Account: Active, Budget: Warning, ValidUntil: valid}, ok: true, warn: true},
		"trial ok":                  {r: Record{Account: Trial, Budget: OK, ValidUntil: valid}, ok: true},
		"past due, in grace":        {r: Record{Account: PastDue, Budget: OK, ValidUntil: valid}, ok: true},
		"capped":                    {r: Record{Account: Active, Budget: Capped, ValidUntil: valid, Reason: "November's cap"}, ok: true, hold: true, why: "spend cap is reached: November's cap"},
		"suspended":                 {r: Record{Account: Suspended, Budget: OK, ValidUntil: valid}, ok: true, hold: true, why: "account is suspended"},
		"expired active: fail open": {r: Record{Account: Active, Budget: OK, ValidUntil: expired}, ok: true, open: true, why: "launching on its last state"},
		"expired active warning":    {r: Record{Account: Active, Budget: Warning, ValidUntil: expired}, ok: true, open: true, warn: true},
		"expired active capped":     {r: Record{Account: Active, Budget: Capped, ValidUntil: expired}, ok: true, hold: true, why: "spend cap"},
		"expired trial":             {r: Record{Account: Trial, Budget: OK, ValidUntil: expired}, ok: true, hold: true, why: "until billing renews it"},
		"expired past due":          {r: Record{Account: PastDue, Budget: OK, ValidUntil: expired}, ok: true, hold: true, why: "account past_due"},
		"expired suspended":         {r: Record{Account: Suspended, Budget: OK, ValidUntil: expired}, ok: true, hold: true, why: "suspended"},
		"expiry is exclusive":       {r: Record{Account: Trial, Budget: OK, ValidUntil: now}, ok: true, hold: true},
	} {
		t.Run(name, func(t *testing.T) {
			d := Decide(tc.r, tc.ok, now)
			if d.Hold != tc.hold || d.FailOpen != tc.open || d.Warning != tc.warn || !strings.Contains(d.Why, tc.why) {
				t.Fatalf("Decide = %+v; want hold %v, fail open %v, warning %v, why containing %q", d, tc.hold, tc.open, tc.warn, tc.why)
			}
		})
	}
}

func TestRecordValidate(t *testing.T) {
	good := Record{Account: Active, Budget: OK, ValidUntil: valid}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	good.Limits = &Limits{MaxRunners: 2, Tiers: []string{"small"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if l := (Record{}).PlanLimits(); l.MaxRunners != 0 || l.Tiers != nil {
		t.Fatalf("a record without limits has %+v", l)
	}
	for name, r := range map[string]Record{
		"account":     {Account: "paid", Budget: OK, ValidUntil: valid},
		"budget":      {Account: Active, Budget: "fine", ValidUntil: valid},
		"valid until": {Account: Active, Budget: OK},
		"runners":     {Account: Active, Budget: OK, ValidUntil: valid, Limits: &Limits{MaxRunners: -1}},
		"no tiers":    {Account: Active, Budget: OK, ValidUntil: valid, Limits: &Limits{Tiers: []string{}}},
		"empty tier":  {Account: Active, Budget: OK, ValidUntil: valid, Limits: &Limits{Tiers: []string{"small", ""}}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: a bad record validated", name)
		}
	}
}

// A file store round-trips records, and a record billing wrote wrong comes
// back Invalid, holding its tenant, rather than failing the whole read.
func TestFileStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entitlements.json")
	s := &FileStore{Path: path}
	ctx := context.Background()
	if rs, err := s.Load(ctx); err != nil || len(rs) != 0 {
		t.Fatalf("missing file: %v, %v", rs, err)
	}
	r := Record{Plan: "team", Account: Active, Budget: Warning, ValidUntil: valid}
	if err := s.Put(ctx, "acme", r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"acme": `+mustJSON(t, r)+`, "bad": {"account_state": "paid"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := rs["acme"]; got.Plan != "team" || got.Budget != Warning || !got.ValidUntil.Equal(valid) {
		t.Fatalf("acme = %+v", got)
	}
	if rs["bad"].Invalid == "" {
		t.Fatalf("bad = %+v, want it invalid", rs["bad"])
	}
	if err := s.Delete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	rs, _ = s.Load(ctx)
	if _, ok := rs["acme"]; ok || rs["bad"].Invalid == "" {
		t.Fatalf("after delete: %+v; want acme gone and the unreadable record kept as written", rs)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"paid"`) {
		t.Fatalf("the unreadable record was rewritten:\n%s", b)
	}
}

func mustJSON(t *testing.T, r Record) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A tenant launching on an expired record is noted from the read that finds
// it so, until a read finds it renewed, and each time it begins is counted.
// The store's write time comes with each record.
func TestWatcherCountsFailOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entitlements.json")
	fs := &FileStore{Path: path}
	put := func(r Record) {
		t.Helper()
		if err := fs.Put(context.Background(), "acme", r); err != nil {
			t.Fatal(err)
		}
	}
	put(Record{Account: Active, Budget: OK, ValidUntil: valid})
	clock := now
	w := &Watcher{Store: fs, now: func() time.Time { return clock }}
	read := func() FailOpen {
		t.Helper()
		if err := w.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
		return w.FailOpen("acme")
	}
	if f := read(); f.Count != 0 || !f.Since.IsZero() {
		t.Fatalf("valid: %+v", f)
	}
	if r, _, _ := w.Decide("acme"); r.Written.IsZero() {
		t.Fatal("the file's write time did not come with the record")
	}
	clock = valid.Add(time.Minute)
	expiredAt := clock
	if f := read(); f.Count != 1 || !f.Since.Equal(expiredAt) {
		t.Fatalf("expired, active: %+v; want 1 since %v", f, expiredAt)
	}
	clock = clock.Add(time.Minute)
	if f := read(); f.Count != 1 || !f.Since.Equal(expiredAt) {
		t.Fatalf("still expired: %+v; want the same episode", f)
	}
	put(Record{Account: Active, Budget: OK, ValidUntil: clock.Add(time.Hour)})
	if f := read(); f.Count != 1 || !f.Since.IsZero() {
		t.Fatalf("renewed: %+v", f)
	}
	clock = clock.Add(2 * time.Hour)
	if f := read(); f.Count != 2 || !f.Since.Equal(clock) {
		t.Fatalf("expired again: %+v; want the second time", f)
	}
	// With the store down, an expiry by the clock alone still begins one.
	put(Record{Account: Active, Budget: OK, ValidUntil: clock.Add(time.Hour)})
	if f := read(); !f.Since.IsZero() {
		t.Fatalf("renewed again: %+v", f)
	}
	w.Store = &failingStore{Store: fs, err: errors.New("throttled")}
	clock = clock.Add(2 * time.Hour)
	if err := w.Read(context.Background()); err == nil {
		t.Fatal("a failed read reported no error")
	}
	if f := w.FailOpen("acme"); f.Count != 3 || !f.Since.Equal(clock) {
		t.Fatalf("expired with the store down: %+v; want the third time", f)
	}
	w.Store = fs
	if err := fs.Delete(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if f := read(); f.Count != 3 || !f.Since.IsZero() {
		t.Fatalf("record gone: %+v; want it no longer launching, counted", f)
	}
}

type failingStore struct {
	Store
	err error
}

func (f *failingStore) Load(ctx context.Context) (Records, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.Store.Load(ctx)
}

// A failed read keeps the last records, and the watcher still calls OnRead
// so the decisions are taken again: an expiry takes effect with no change in
// the store, and with the store unreachable.
func TestWatcherKeepsRecordsAndDecidesByTheClock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entitlements.json")
	fs := &FileStore{Path: path}
	if err := fs.Put(context.Background(), "acme", Record{Account: Trial, Budget: OK, ValidUntil: valid}); err != nil {
		t.Fatal(err)
	}
	store := &failingStore{Store: fs}
	clock := now
	reads := 0
	w := &Watcher{Store: store, now: func() time.Time { return clock }, OnRead: func() { reads++ }}
	if err := w.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, d := w.Decide("acme"); !ok || d.Hold {
		t.Fatalf("valid trial: ok %v, %+v", ok, d)
	}
	store.err = errors.New("AccessDeniedException")
	clock = valid.Add(time.Second)
	if err := w.Read(context.Background()); err == nil {
		t.Fatal("a failed read reported no error")
	}
	if reads != 2 {
		t.Fatalf("OnRead called %d times, want after each read", reads)
	}
	if _, ok, d := w.Decide("acme"); !ok || !d.Hold {
		t.Fatalf("expired trial with the store down: ok %v, %+v; want the kept record, held", ok, d)
	}
	if f := w.FailOpen("acme"); f.Count != 0 || !f.Since.IsZero() {
		t.Fatalf("a held trial launches on nothing: %+v", f)
	}
	if st := w.Status(); !st.Known || st.LastError == "" {
		t.Fatalf("status %+v", st)
	}
	if _, ok, d := w.Decide("other"); ok || !d.Hold {
		t.Fatalf("a tenant with no record: ok %v, %+v", ok, d)
	}
}
