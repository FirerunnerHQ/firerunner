package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

var overviewNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time  { return overviewNow.Add(-d) }
func from(d time.Duration) time.Time { return overviewNow.Add(d) }

// sampleOverview is two live shards, one stale, and a tenant in each state.
func sampleOverview(t *testing.T) jobstore.Overview {
	t.Helper()
	big := listed("big", 2)
	big.Tiers = make([]string, tenantMaxTiers+1)
	for i := range big.Tiers {
		big.Tiers[i] = "t"
	}
	list, err := json.Marshal([]config.Tenant{listed("served", 2, "x", "y"), listed("moving", 2, "x"), listed("waiting", 2, "x"), listed("new", 2, "x"), big})
	if err != nil {
		t.Fatal(err)
	}
	gone, _ := json.Marshal(listed("gone", 2, "x"))
	return jobstore.Overview{
		Now:  overviewNow,
		List: list,
		Shards: []jobstore.ShardRow{
			{ID: "s1", Host: "vm-1", Owner: "vm-1", TenantOwner: "fr", HeartbeatAt: ago(3 * time.Second), ListOK: true, Live: true},
			{ID: "s2", Host: "vm-2", Owner: "vm-2", TenantOwner: "fr", HeartbeatAt: ago(5 * time.Second), ListOK: true, Live: true},
			{ID: "s3", Host: "vm-3", Owner: "vm-3", TenantOwner: "fr", HeartbeatAt: ago(2 * time.Minute), ListOK: true},
		},
		Leases: []jobstore.LeaseRow{
			{Account: jobstore.AssignerLease, Shard: "s1", ExpiresAt: from(25 * time.Second)},
			{Account: acct("served"), Shard: "s1", ExpiresAt: from(25 * time.Second)},
			{Account: acct("moving"), Shard: "s3", ExpiresAt: from(10 * time.Second)},
			{Account: acct("waiting"), Shard: "s2", ExpiresAt: ago(time.Minute)}, // expired
			{Account: acct("gone"), Shard: "s2", ExpiresAt: from(20 * time.Second)},
		},
		Assignments: []jobstore.AssignmentRow{
			{Account: acct("served"), Shard: "s1", Tiers: 2},
			{Account: acct("moving"), Shard: "s2", Tiers: 1},
			{Account: acct("waiting"), Shard: "s3", Tiers: 1},
		},
		Removed: []jobstore.Removed{{ID: "gone", Entry: gone, RemovedAt: ago(20 * time.Minute)}},
	}
}

// Each tenant's state says who serves it, or why none does; each shard's,
// what it is and carries; and what needs looking at is listed.
func TestShardsOf(t *testing.T) {
	r, err := shardsOf(sampleOverview(t))
	if err != nil {
		t.Fatal(err)
	}
	if r.Assigner != "s1" || !r.Shards[0].Assigner {
		t.Fatalf("assigner %q, shards %+v", r.Assigner, r.Shards)
	}
	states := map[string]string{}
	for _, s := range r.Shards {
		states[s.ID] = s.State
	}
	if want := map[string]string{"s1": "live", "s2": "live", "s3": "stale"}; !maps.Equal(states, want) {
		t.Fatalf("shard states %v, want %v", states, want)
	}
	if s := r.Shards[0]; s.Tenants != 1 || s.Tiers != 2 {
		t.Fatalf("s1 carries %d tenant(s), %d tier(s), want 1 and 2", s.Tenants, s.Tiers)
	}
	byID := map[string]shardsTenant{}
	for _, tn := range r.Tenants {
		byID[tn.ID] = tn
	}
	for _, c := range []struct{ id, state, why string }{
		{"served", "served", ""},
		{"moving", "moving", "s3 still holds its lease"},
		{"waiting", "waiting", "s3, which is not live"},
		{"new", "unplaced", "not assigned yet"},
		{"big", "unplaced", "41 tiers"},
	} {
		got := byID[c.id]
		if got.State != c.state || !strings.Contains(got.Why, c.why) {
			t.Errorf("%s: %s (%s), want %s (%s)", c.id, got.State, got.Why, c.state, c.why)
		}
	}
	if byID["served"].LeasedBy != "s1" || byID["waiting"].LeasedBy != "" {
		t.Fatalf("leased by %q and %q: an expired lease is no one's", byID["served"].LeasedBy, byID["waiting"].LeasedBy)
	}
	if len(r.Removals) != 1 || r.Removals[0].LeasedBy != "s2" || r.Removals[0].Account != acct("gone") {
		t.Fatalf("removals %+v", r.Removals)
	}
	// Four tenants not served, and a removal not done after 15 minutes.
	if len(r.Problems) != 5 || !slices.ContainsFunc(r.Problems, func(p string) bool { return strings.HasPrefix(p, "removal of tenant gone") }) {
		t.Fatalf("problems %q", r.Problems)
	}
}

// With no assigner, and a shard whose list did not apply, both are named.
func TestShardsOfWithNoAssigner(t *testing.T) {
	o := sampleOverview(t)
	o.Leases = o.Leases[1:]
	o.Shards[1].ListOK, o.Shards[1].Live = false, false
	r, err := shardsOf(o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.Problems, "\n")
	for _, want := range []string{"no shard holds the assigner's lease", "shard s2: its last read of the tenants list did not apply"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("problems %q, want %q", r.Problems, want)
		}
	}
	if r.Shards[1].State != "list-failed" {
		t.Fatalf("s2 %s, want list-failed", r.Shards[1].State)
	}
}

// An empty database: no shard, no tenant, no problem.
func TestShardsOfNothing(t *testing.T) {
	r, err := shardsOf(jobstore.Overview{Now: overviewNow, List: json.RawMessage("[]")})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 0 || len(r.Shards) != 0 {
		t.Fatalf("report %+v", r)
	}
	var out bytes.Buffer
	r.render(&out)
	if !strings.Contains(out.String(), "none: no daemon serves this list") || !strings.Contains(out.String(), "no problem") {
		t.Fatalf("render:\n%s", out.String())
	}
}

type fakeShards struct {
	o      jobstore.Overview
	err    error
	closed bool
}

func (f *fakeShards) Overview(context.Context) (jobstore.Overview, error) { return f.o, f.err }
func (f *fakeShards) Close()                                              { f.closed = true }

func runShards(t *testing.T, store *fakeShards, args ...string) (int, string, string) {
	t.Helper()
	listInstall(t)
	var out, errOut bytes.Buffer
	code := tenantCmd(append(append([]string{"shards"}, args...), "--env-file", "none"), &out, &errOut, tenantOpeners{
		shards: func(*config.Config) (shardsStore, error) { return store, nil },
	})
	return code, out.String(), errOut.String()
}

// The command prints the report for people, or as JSON; a bad argument is
// a usage error, and a store that fails, a failure.
func TestTenantShardsCmd(t *testing.T) {
	store := &fakeShards{o: sampleOverview(t)}
	code, out, errOut := runShards(t, store)
	if code != 0 || !store.closed {
		t.Fatalf("exit %d, closed %v: %s", code, store.closed, errOut)
	}
	for _, want := range []string{"s1  live, assigning", "served  " + acct("served") + "  2 tier(s)  served by s1", "big  ", "unplaced: it lists 41 tiers", "removals not yet cleaned up:", "problems:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}

	code, out, errOut = runShards(t, &fakeShards{o: sampleOverview(t)}, "--json")
	var r shardsReport
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Assigner != "s1" || len(r.Tenants) != 5 {
		t.Fatalf("--json: exit %d, %s %s", code, out, errOut)
	}

	if code, out, _ = runShards(t, &fakeShards{}, "--help"); code != 0 || !strings.Contains(out, "firerunner tenant shards") {
		t.Fatalf("--help: exit %d, %s", code, out)
	}
	if code, _, errOut = runShards(t, &fakeShards{}, "--bogus"); code != 2 || !strings.Contains(errOut, `unknown argument "--bogus"`) {
		t.Fatalf("bad argument: exit %d, %s", code, errOut)
	}
	if code, _, errOut = runShards(t, &fakeShards{err: errors.New("connection refused")}); code != 1 || !strings.Contains(errOut, "connection refused") {
		t.Fatalf("store down: exit %d, %s", code, errOut)
	}
}
