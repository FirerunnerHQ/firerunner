package onboard

import (
	"context"
	"testing"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// A repository GitHub reports without a visibility counts as public unless
// it is private: never served by mistake.
func TestRepositoryPublic(t *testing.T) {
	for _, c := range []struct {
		r    Repository
		want bool
	}{
		{Repository{Visibility: "public"}, true},
		{Repository{Visibility: "private", Private: true}, false},
		{Repository{Visibility: "internal"}, false},
		{Repository{Private: false}, true},
		{Repository{Private: true}, false},
	} {
		if got := c.r.public(); got != c.want {
			t.Errorf("%+v: public %v, want %v", c.r, got, c.want)
		}
	}
}

// A group of the right visibility that lets public repositories in is put
// right too.
func TestSyncGroupClosesToPublic(t *testing.T) {
	f, gh := newAPI(t)
	f.groups = []RunnerGroup{{ID: 5, Name: "firerunner", Visibility: "selected", AllowsPublicRepositories: true}}
	f.repos = []Repository{repo(1, "app", "private")}
	if _, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner"); err != nil {
		t.Fatal(err)
	}
	if f.groups[0].AllowsPublicRepositories {
		t.Fatal("the group still lets public repositories in")
	}
}

// Without Lookup, an installation that is not active is not kept.
func TestKeeperSkipsInactive(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	store := &groupStore{results: map[int64]GroupResult{}, rows: map[int64]jobstore.Installation{
		9: {ID: 9, AccountID: 902, AccountLogin: "acme", State: "suspended"},
	}}
	if _, kept, err := (Keeper{Store: store, GitHub: gh, Group: "firerunner"}).Keep(context.Background(), 9); kept || err != nil || len(f.requests) != 0 {
		t.Fatalf("kept %v, %v, requests %v", kept, err, f.requests)
	}
}
