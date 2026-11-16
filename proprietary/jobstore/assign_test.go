package jobstore

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

// Only the holder of the assigner's lease writes the assignments; each
// shard then renews the leases of the accounts assigned to it, and lets go
// of one assigned elsewhere.
func TestAssignIsTheAssignersAndRenewFollowsIt(t *testing.T) {
	s, dsn, sh := joined(t, "a", "b")
	ctx := context.Background()
	held(t, s, sh[0], []string{AssignerLease}, AssignerLease)
	held(t, s, sh[1], nil, AssignerLease)

	next := map[string]Assignment{"x": {Shard: "a", Tiers: 3}, "y": {Shard: "b", Tiers: 2}}
	if err := s.Assign(ctx, sh[1], next); !errors.Is(err, ErrNotAssigner) {
		t.Fatalf("Assign by a shard without the lease = %v, want ErrNotAssigner", err)
	}
	if err := s.Assign(ctx, sh[0], next); err != nil {
		t.Fatal(err)
	}
	held(t, s, sh[0], []string{AssignerLease, "x"}, AssignerLease)
	held(t, s, sh[1], []string{"y"})

	// x moves to b: a lets go of it at its next renewal, and b takes it.
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"x": {Shard: "b", Tiers: 3}, "y": {Shard: "b", Tiers: 2}}); err != nil {
		t.Fatal(err)
	}
	held(t, s, sh[1], []string{"y"})
	held(t, s, sh[0], []string{AssignerLease}, AssignerLease)
	held(t, s, sh[1], []string{"x", "y"})

	// The assigner's lease expired: its holder may not assign.
	exec(t, dsn, `UPDATE leases SET expires_at = now() - interval '1 second' WHERE account = $1`, AssignerLease)
	if err := s.Assign(ctx, sh[0], nil); !errors.Is(err, ErrNotAssigner) {
		t.Fatalf("Assign past the lease = %v, want ErrNotAssigner", err)
	}
}

// A shard keeps the lease of an account it runs until that account is
// assigned to another shard: unassigned (dropped from the assigner's list
// before its own), it still holds it. Of the accounts assigned to it, it
// takes only those its list names, and says which it does not.
func TestRenewKeepsWhatItRunsAndTakesWhatItLists(t *testing.T) {
	s, _, sh := joined(t, "a", "b")
	ctx := context.Background()
	held(t, s, sh[0], []string{AssignerLease}, AssignerLease)
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"x": {Shard: "b", Tiers: 1}, "y": {Shard: "b", Tiers: 1}}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Renew(ctx, sh[1], Want{Listed: []string{"x"}, ListOK: true}, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Held, []string{"x"}) || !slices.Equal(r.Unlisted, []string{"y"}) || r.Assigner != "a" {
		t.Fatalf("renewal %+v, want x held, y unlisted, a assigning", r)
	}

	// x unassigned: b, which runs it, keeps it.
	held(t, s, sh[0], []string{AssignerLease}, AssignerLease)
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"y": {Shard: "b", Tiers: 1}}); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Renew(ctx, sh[1], Want{Listed: []string{"x", "y"}, Running: []string{"x"}, ListOK: true}, testTTL); err != nil {
		t.Fatal(err)
	}
	slices.Sort(r.Held)
	if !slices.Equal(r.Held, []string{"x", "y"}) {
		t.Fatalf("held %v, want x kept while it runs, and y", r.Held)
	}

	// x assigned to a: b lets go of it, running or not, and a takes it.
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"x": {Shard: "a", Tiers: 1}, "y": {Shard: "b", Tiers: 1}}); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Renew(ctx, sh[1], Want{Listed: []string{"x", "y"}, Running: []string{"x"}, ListOK: true}, testTTL); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Held, []string{"y"}) {
		t.Fatalf("held %v, want x let go once assigned elsewhere", r.Held)
	}
	held(t, s, sh[0], []string{AssignerLease, "x"}, AssignerLease)
}

// A shard that drains lets go of the assigner's lease at once.
func TestDrainReleasesTheAssignersLease(t *testing.T) {
	s, _, sh := joined(t, "a", "b")
	held(t, s, sh[0], []string{AssignerLease}, AssignerLease)
	held(t, s, sh[1], nil, AssignerLease)
	if err := s.Drain(context.Background(), sh[0]); err != nil {
		t.Fatal(err)
	}
	held(t, s, sh[1], []string{AssignerLease}, AssignerLease)
}

// Assign rewrites only what changed: assigned_at says when an account last
// moved. An account left out is unassigned.
func TestAssignKeepsWhatDidNotMove(t *testing.T) {
	s, dsn, sh := joined(t, "a", "b")
	ctx := context.Background()
	held(t, s, sh[0], []string{AssignerLease}, AssignerLease)
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"x": {Shard: "a", Tiers: 1}, "y": {Shard: "a", Tiers: 1}}); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE assignments SET assigned_at = now() - interval '1 hour'`)
	if err := s.Assign(ctx, sh[0], map[string]Assignment{"x": {Shard: "a", Tiers: 4}, "z": {Shard: "b", Tiers: 1}}); err != nil {
		t.Fatal(err)
	}
	if age := query[float64](t, dsn, `SELECT extract(epoch FROM now() - assigned_at) FROM assignments WHERE account = 'x'`); age < 3000 {
		t.Fatalf("x moved nowhere, but assigned_at is %.0fs old", age)
	}
	if n := query[int64](t, dsn, `SELECT tiers FROM assignments WHERE account = 'x'`); n != 4 {
		t.Fatalf("x has %d tiers, want 4", n)
	}
	_, got, _, err := s.Placement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]Assignment{"x": {Shard: "a", Tiers: 4}, "z": {Shard: "b", Tiers: 1}}; !maps.Equal(got, want) {
		t.Fatalf("assignments %v, want %v", got, want)
	}
}

// Placement tells live shards from those draining or no longer beating.
func TestPlacementShards(t *testing.T) {
	s, dsn, sh := joined(t, "a", "b", "c", "d")
	ctx := context.Background()
	for _, x := range sh {
		renew(t, s, x)
	}
	if _, err := s.Renew(ctx, sh[3], Want{ListOK: false}, testTTL); err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(ctx, sh[1]); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE shards SET heartbeat_at = now() - make_interval(secs => $1) WHERE id = 'c'`, (ShardStale + time.Second).Seconds())
	held(t, s, sh[0], []string{"x"}, "x")
	shards, _, holders, err := s.Placement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(holders, map[string]string{"x": "a"}) {
		t.Fatalf("holders %v, want x on a", holders)
	}
	want := []ShardState{{ID: "a", ListOK: true, Live: true}, {ID: "b", Draining: true, ListOK: true}, {ID: "c", ListOK: true}, {ID: "d"}}
	if len(shards) != len(want) {
		t.Fatalf("shards %+v, want %+v", shards, want)
	}
	for i := range want {
		if shards[i] != want[i] {
			t.Fatalf("shards %+v, want %+v", shards, want)
		}
	}
}
