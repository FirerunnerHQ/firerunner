package main

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

func live(ids ...string) []jobstore.ShardState {
	var s []jobstore.ShardState
	for _, id := range ids {
		s = append(s, jobstore.ShardState{ID: id, Live: true})
	}
	return s
}

func on(shard string, tiers int) jobstore.Assignment {
	return jobstore.Assignment{Shard: shard, Tiers: tiers}
}

type placed = map[string]jobstore.Assignment

func TestPlace(t *testing.T) {
	for _, c := range []struct {
		name     string
		tenants  []tenantPlace
		shards   []jobstore.ShardState
		current  placed
		holders  map[string]string
		want     placed
		unplaced map[string]string // id -> a word of why
	}{
		{
			name:    "fewest tiers first, the first by id on a tie",
			tenants: []tenantPlace{{"a", "A", 3}, {"b", "B", 2}, {"c", "C", 1}},
			shards:  live("s2", "s1"),
			want:    placed{"A": on("s1", 3), "B": on("s2", 2), "C": on("s2", 1)},
		},
		{
			name:    "a tenant stays on its live shard, however loaded",
			tenants: []tenantPlace{{"a", "A", 30}, {"b", "B", 30}, {"c", "C", 1}},
			shards:  live("s1", "s2"),
			current: placed{"A": on("s1", 30), "B": on("s1", 30)},
			want:    placed{"A": on("s1", 30), "B": on("s1", 30), "C": on("s2", 1)},
		},
		{
			name:    "off a shard gone or draining, onto the live ones",
			tenants: []tenantPlace{{"a", "A", 2}, {"b", "B", 2}, {"c", "C", 2}},
			shards:  []jobstore.ShardState{{ID: "gone"}, {ID: "s1", Live: true}, {ID: "drain", Draining: true}},
			current: placed{"A": on("gone", 2), "B": on("drain", 2), "C": on("unknown", 2)},
			want:    placed{"A": on("s1", 2), "B": on("s1", 2), "C": on("s1", 2)},
		},
		{
			name:    "an account unassigned stays with the live shard that holds its lease",
			tenants: []tenantPlace{{"a", "A", 1}, {"b", "B", 1}, {"c", "C", 1}},
			shards:  append(live("s1", "s2"), jobstore.ShardState{ID: "gone"}),
			current: placed{"C": on("gone", 1)},
			holders: map[string]string{"A": "s2", "B": "gone", "C": "s2"},
			want:    placed{"A": on("s2", 1), "B": on("s1", 1), "C": on("s2", 1)},
		},
		{
			name:    "a shard whose list did not apply takes nothing new",
			tenants: []tenantPlace{{"a", "A", 1}},
			shards:  []jobstore.ShardState{{ID: "s0", ListOK: false}, {ID: "s1", ListOK: true, Live: true}},
			want:    placed{"A": on("s1", 1)},
		},
		{
			name:    "a tier count that changed is kept up to date in place",
			tenants: []tenantPlace{{"a", "A", 5}},
			shards:  live("s1", "s2"),
			current: placed{"A": on("s2", 1)},
			want:    placed{"A": on("s2", 5)},
		},
		{
			name:    "an account no longer listed is unassigned",
			tenants: []tenantPlace{{"a", "A", 1}},
			shards:  live("s1"),
			current: placed{"A": on("s1", 1), "GONE": on("s1", 4)},
			want:    placed{"A": on("s1", 1)},
		},
		{
			name:     "a tenant over its tier cap is placed nowhere, even where it was",
			tenants:  []tenantPlace{{"big", "BIG", tenantMaxTiers + 1}, {"a", "A", 1}},
			shards:   live("s1"),
			current:  placed{"BIG": on("s1", tenantMaxTiers+1)},
			want:     placed{"A": on("s1", 1)},
			unplaced: map[string]string{"big": "41 tiers"},
		},
		{name: "a shard takes no more than its cap", shards: live("s1")}, // filled below
		{
			name:     "no live shard",
			tenants:  []tenantPlace{{"a", "A", 1}},
			shards:   []jobstore.ShardState{{ID: "s1", Draining: true}},
			want:     placed{},
			unplaced: map[string]string{"a": "no shard is live"},
		},
	} {
		if c.name == "a shard takes no more than its cap" {
			// Fill s1 to the cap with tenants that stay, then one more.
			c.tenants, c.current, c.want = nil, placed{}, placed{}
			for i := range shardMaxTiers / 25 {
				acc := string(rune('A' + i))
				c.tenants = append(c.tenants, tenantPlace{strings.ToLower(acc), acc, 25})
				c.current[acc] = on("s1", 25)
				c.want[acc] = on("s1", 25)
			}
			c.tenants = append(c.tenants, tenantPlace{"more", "zz", 1})
			c.unplaced = map[string]string{"more": "no live shard has room"}
		}
		got, unplaced := place(c.tenants, c.shards, c.current, c.holders)
		if !maps.Equal(got, c.want) {
			t.Errorf("%s: placed %v, want %v", c.name, got, c.want)
		}
		if len(unplaced) != len(c.unplaced) {
			t.Errorf("%s: unplaced %v, want %v", c.name, unplaced, c.unplaced)
		}
		for id, word := range c.unplaced {
			if !strings.Contains(unplaced[id], word) {
				t.Errorf("%s: %s unplaced as %q, want it to say %q", c.name, id, unplaced[id], word)
			}
		}
	}
}

// The shard that holds the assigner's lease places the listed tenants after
// it renews, and renews again when anything moved, so that it takes its
// own new accounts at once. One that does not hold it, or has read no list,
// assigns nothing.
func TestLeaserAssigns(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	store.shards = live("s1", "s2")
	lr.joined = true

	// No list yet: nothing is assigned, as every tenant would read removed
	// and lose its shard, as after a restart.
	store.set(jobstore.AssignerLease)
	store.grant = append(store.grant, acct("a"), acct("b")) // free, not assigned
	store.assignments = placed{acct("a"): on("s1", 3)}
	if err := lr.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.assigns != 0 {
		t.Fatal("assigned before a list was read")
	}

	big := listed("big", 2)
	big.Tiers = make([]string, tenantMaxTiers+1)
	lr.apply([]config.Tenant{listed("a", 2, "x", "y", "z"), listed("b", 2, "x"), big})
	_, before := store.counts()
	if err := lr.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := placed{acct("a"): on("s1", 3), acct("b"): on("s2", 1)}
	if !maps.Equal(store.assignments, want) {
		t.Fatalf("assigned %v, want %v", store.assignments, want)
	}
	if _, after := store.counts(); after != before+2 {
		t.Fatalf("%d renewals, want 2: one more for what moved", after-before)
	}
	l.started("a") // its own, taken at once; b is s2's
	st := lr.status()
	if !st.Assigner || st.AssignerShard != "s1" || st.Tenants != 1 || len(st.Unplaced) != 1 || !strings.HasPrefix(st.Unplaced[0], "big: ") {
		t.Fatalf("status %+v, want the assigner holding a, with big unplaced", st)
	}

	// Nothing moved: one renewal, no write.
	_, before = store.counts()
	if err := lr.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, after := store.counts(); store.assigns != 1 || after != before+1 {
		t.Fatalf("%d assigns and %d renewals after nothing moved, want 1 and 1", store.assigns, after-before)
	}

	// A failed write is the step's error, tried again.
	store.shards = live("s2")
	store.assignErr = jobstore.ErrNotAssigner
	if err := lr.step(context.Background()); !errors.Is(err, jobstore.ErrNotAssigner) {
		t.Fatalf("step = %v, want ErrNotAssigner", err)
	}
	if st := lr.status(); st.Failing() || !strings.Contains(st.AssignError, "assigner's lease") {
		t.Fatalf("status %+v, want an assignment error, not a renewal's", st)
	}

	// The lease gone to another shard: this one assigns nothing.
	store.assignErr = nil
	store.set()
	if err := lr.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := lr.status(); st.Assigner || len(st.Unplaced) != 0 || store.assigns != 1 {
		t.Fatalf("status %+v, %d assigns, after the assigner's lease went", st, store.assigns)
	}
}

// A tenant the assigner unassigns while this shard serves it, as when the
// assigner reads a list without it first, keeps its lease here: when this
// shard's list drops it too, it is removed, its scale sets deleted, not
// handed off and left registered.
func TestLeaserKeepsATenantUnassignedUntilItsListSays(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	lr.apply([]config.Tenant{listed("a", 2, "x")})
	store.set(acct("a"))
	mustRenew(t, lr)
	l.started("a")

	delete(store.assignments, acct("a")) // the assigner's list lacks a
	before := l.runs()
	mustRenew(t, lr)
	l.unchanged(before)

	lr.apply(nil)
	l.ended("a", errTenantRemoved)
	l.finish("a")
	l.set.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.removed) != 1 {
		t.Fatalf("deleted %v, want a's", l.removed)
	}
}

// An account assigned to this shard that its own list does not name is
// not taken, and status says so; nor is the shard a place for new tenants
// while its list does not apply.
func TestLeaserTakesOnlyWhatItsListNames(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	lr.apply([]config.Tenant{listed("a", 2, "x")})
	store.set(acct("a"), acct("new"))
	mustRenew(t, lr)
	l.started("a")
	if st := lr.status(); st.Tenants != 1 || len(st.Unlisted) != 1 || st.Unlisted[0] != acct("new") {
		t.Fatalf("status %+v, want new unlisted", st)
	}
	store.mu.Lock()
	ok := store.wants[len(store.wants)-1].ListOK
	store.mu.Unlock()
	if !ok {
		t.Fatal("a shard whose list applied says it did not")
	}
	lr.listFailed()
	mustRenew(t, lr)
	store.mu.Lock()
	ok = store.wants[len(store.wants)-1].ListOK
	store.mu.Unlock()
	if ok {
		t.Fatal("a shard whose list did not apply says it did")
	}
}

// A stopping shard marks itself draining, for the assigner to move its
// tenants now.
func TestLeaserDrain(t *testing.T) {
	_, _, store, lr := leaserLab(t)
	lr.drain(context.Background())
	if len(store.drained) != 1 || store.drained[0] != "s1" {
		t.Fatalf("drained %v", store.drained)
	}
}
