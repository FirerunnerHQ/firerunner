package entitlement

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// fakeDB keeps rows as the job store's entitlements table would.
type fakeDB struct {
	rows map[string]jobstore.Entitlement
	err  error
}

func (f *fakeDB) Entitlements(context.Context) ([]jobstore.Entitlement, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []jobstore.Entitlement
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeDB) PutEntitlement(_ context.Context, tenant string, record json.RawMessage) error {
	if f.rows == nil {
		f.rows = map[string]jobstore.Entitlement{}
	}
	f.rows[tenant] = jobstore.Entitlement{Tenant: tenant, Record: record, UpdatedAt: time.Now()}
	return nil
}

func (f *fakeDB) DeleteEntitlement(_ context.Context, tenant string) error {
	delete(f.rows, tenant)
	return nil
}

// A database store reads each tenant's row with when it was last written;
// a row billing wrote wrong holds its tenant, as in every store; what Put
// writes, Load reads back.
func TestDBStore(t *testing.T) {
	written := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	db := &fakeDB{rows: map[string]jobstore.Entitlement{
		"acme": {Tenant: "acme", UpdatedAt: written,
			Record: json.RawMessage(`{"account_state":"active","budget_state":"ok","valid_until":"2026-12-01T00:00:00Z","limits":{"tiers":["small"]}}`)},
		"bad": {Tenant: "bad", UpdatedAt: written, Record: json.RawMessage(`{"account_state":"gold","budget_state":"ok","valid_until":"2026-12-01T00:00:00Z"}`)},
	}}
	s, err := Open(DBSpec, nil, func() (DB, error) { return db, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.String(), "db:") {
		t.Errorf("String = %q, want it to name db:", s)
	}
	ctx := context.Background()
	rs, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := rs["acme"]; r.Invalid != "" || !r.Written.Equal(written) || len(r.PlanLimits().Tiers) != 1 {
		t.Fatalf("acme = %+v", r)
	}
	if d := Decide(rs["bad"], true, written); !d.Hold || !strings.Contains(d.Why, "does not parse") {
		t.Fatalf("an invalid row decides %+v, want held", d)
	}
	until := written.Add(time.Hour)
	if err := s.Put(ctx, "beta", Record{Account: Trial, Budget: Warning, ValidUntil: until}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "bad"); err != nil {
		t.Fatal(err)
	}
	rs, err = s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rs["bad"]; ok || rs["beta"].Account != Trial || !rs["beta"].ValidUntil.Equal(until) {
		t.Fatalf("after put and delete: %+v", rs)
	}

	db.err = errors.New("connection refused")
	if _, err := s.Load(ctx); err == nil {
		t.Fatal("a failed read was not an error: the watcher would take it for no records")
	}
}

// A record's pool names tiers and sizes from 0; it reads back as written.
func TestLimitsPool(t *testing.T) {
	until := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	ok := Record{Account: Active, Budget: OK, ValidUntil: until, Limits: &Limits{Pool: map[string]int{"small": 3, "big": 0}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a pool: %v", err)
	}
	for name, min := range map[string]map[string]int{"negative min": {"small": -1}, "min no tier": {"": 1}} {
		r := ok
		r.Limits = &Limits{Min: min}
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "limits.min") {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	for name, pool := range map[string]map[string]int{"negative": {"small": -1}, "no tier": {"": 1}} {
		r := ok
		r.Limits = &Limits{Pool: pool}
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "limits.pool") {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	r := decode([]byte(`{"account_state":"active","budget_state":"ok","valid_until":"2026-12-01T00:00:00Z","limits":{"pool":{"small":3}}}`))
	if r.Invalid != "" || r.PlanLimits().Pool["small"] != 3 {
		t.Fatalf("decoded %+v", r)
	}
}

// db: names the job store alone; anything after it is a mistake, and an
// unknown spec is refused as before. The job store is asked for only for
// db:.
func TestCheckSpecDB(t *testing.T) {
	if err := CheckSpec("db:"); err != nil {
		t.Errorf("db: = %v", err)
	}
	for _, bad := range []string{"db:entitlements", "db:/x", "relative/path"} {
		if err := CheckSpec(bad); err == nil {
			t.Errorf("CheckSpec(%q) accepted", bad)
		}
	}
	asked := false
	if _, err := Open("/var/lib/firerunner/entitlements.json", nil, func() (DB, error) { asked = true; return nil, nil }); err != nil || asked {
		t.Fatalf("a file spec: err %v, asked the job store %v", err, asked)
	}
	if _, err := Open(DBSpec, nil, func() (DB, error) { return nil, errors.New("no URL") }); err == nil {
		t.Fatal("db: with no job store opened")
	}
}
