package jobstore

import (
	"context"
	"errors"
	"maps"
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
	_, got, err := s.Placement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]Assignment{"x": {Shard: "a", Tiers: 4}, "z": {Shard: "b", Tiers: 1}}; !maps.Equal(got, want) {
		t.Fatalf("assignments %v, want %v", got, want)
	}
}

// Placement tells live shards from those draining or no longer beating.
func TestPlacementShards(t *testing.T) {
	s, dsn, sh := joined(t, "a", "b", "c")
	ctx := context.Background()
	if err := s.Drain(ctx, sh[1]); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE shards SET heartbeat_at = now() - make_interval(secs => $1) WHERE id = 'c'`, (ShardStale + time.Second).Seconds())
	shards, _, err := s.Placement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ShardState{{ID: "a", Live: true}, {ID: "b", Draining: true}, {ID: "c"}}
	if len(shards) != len(want) {
		t.Fatalf("shards %+v, want %+v", shards, want)
	}
	for i := range want {
		if shards[i] != want[i] {
			t.Fatalf("shards %+v, want %+v", shards, want)
		}
	}
}
