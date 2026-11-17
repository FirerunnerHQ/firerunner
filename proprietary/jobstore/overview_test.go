package jobstore

import (
	"context"
	"testing"
)

// Overview reads nothing but empty tables on a database not set up, and
// every table the shards keep once they are, at one moment.
func TestOverview(t *testing.T) {
	s, dsn := openTenantStore(t)
	ctx := context.Background()
	o, err := s.Overview(ctx)
	if err != nil {
		t.Fatalf("on an empty database: %v", err)
	}
	if string(o.List) != "[]" || len(o.Shards)+len(o.Leases)+len(o.Assignments)+len(o.Removed) != 0 || o.Now.IsZero() {
		t.Fatalf("overview of an empty database: %+v", o)
	}

	if err := s.Replace(ctx, []Entry{entry("a", "https://github.com/a"), entry("b", "https://github.com/b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, []Entry{entry("a", "https://github.com/a")}); err != nil {
		t.Fatal(err)
	}
	sh := []Shard{{ID: "s1", Host: "h1", Owner: "vm-1", TenantOwner: "fr"}, {ID: "s2", Host: "h2", Owner: "vm-2", TenantOwner: "fr"}}
	for _, x := range sh {
		if err := s.Join(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Renew(ctx, sh[0], Want{Accounts: []string{AssignerLease, "github.com/a"}, ListOK: true}, testTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Renew(ctx, sh[1], Want{ListOK: true}, testTTL); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"github.com/a": {Shard: "s1", Tiers: 1}}); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE shards SET draining = true WHERE id = 's2'`)

	o, err = s.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// s2 beats and applied its list, but drains: not live.
	if len(o.Shards) != 2 || !o.Shards[0].Live || o.Shards[1].Live || !o.Shards[1].Draining || !o.Shards[1].ListOK || o.Shards[0].TenantOwner != "fr" {
		t.Fatalf("shards %+v", o.Shards)
	}
	if len(o.Leases) != 2 || o.Leases[0].Account != AssignerLease || o.Leases[1].Account != "github.com/a" || o.Leases[1].Shard != "s1" || !o.Leases[1].ExpiresAt.After(o.Now) {
		t.Fatalf("leases %+v", o.Leases)
	}
	if len(o.Assignments) != 1 || o.Assignments[0].Shard != "s1" || o.Assignments[0].Tiers != 1 {
		t.Fatalf("assignments %+v", o.Assignments)
	}
	if len(o.Removed) != 1 || o.Removed[0].ID != "b" {
		t.Fatalf("removed %+v", o.Removed)
	}
	if !jsonHas(o.List, `"id": "a"`) || jsonHas(o.List, `"id": "b"`) {
		t.Fatalf("list %s", o.List)
	}
}
