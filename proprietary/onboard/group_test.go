package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

func repo(id int64, name, visibility string) Repository {
	return Repository{ID: id, FullName: "acme/" + name, Private: visibility != "public", Visibility: visibility}
}

func writes(f *fakeAPI) []string {
	var out []string
	for _, r := range f.requests {
		if !strings.HasPrefix(r, "GET ") && !strings.Contains(r, "access_tokens") {
			out = append(out, strings.SplitN(r, "?", 2)[0])
		}
	}
	return out
}

// An installation of some repositories gets a group open to those that
// are not public, created as such.
func TestSyncGroupCreates(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private"), repo(2, "site", "public"), repo(3, "infra", "internal")}
	res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "ready" || res.GroupID != 100 || res.All || res.Served != 2 || !slices.Equal(res.Public, []string{"acme/site"}) {
		t.Fatalf("result %+v", res)
	}
	if g := f.groups[0]; g.Name != "firerunner" || g.Visibility != "selected" || !slices.Equal(f.groupRepos[100], []int64{1, 3}) {
		t.Fatalf("group %+v, repositories %v", g, f.groupRepos[100])
	}
}

// An installation of all repositories gets a group open to every one that
// is not public, so a new repository is served at once.
func TestSyncGroupAll(t *testing.T) {
	f, gh := newAPI(t)
	f.selection = "all"
	f.repos = []Repository{repo(1, "app", "private"), repo(2, "site", "public")}
	res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 0)
	if err != nil || res.State != "ready" || !res.All || res.Served != 1 {
		t.Fatalf("result %+v, %v", res, err)
	}
	if g := f.groups[0]; g.Visibility != "private" {
		t.Fatalf("group %+v", g)
	}
	if _, sent := f.bodies["POST /orgs/acme/actions/runner-groups"]["selected_repository_ids"]; sent {
		t.Fatal("repositories selected for a group open to all")
	}
}

// The group onboarding created is put right: its name, its visibility,
// public repositories kept out, and exactly the installation's
// repositories, one removed from the installation included.
func TestSyncGroupUpdates(t *testing.T) {
	f, gh := newAPI(t)
	f.groups = []RunnerGroup{{ID: 1, Name: "Default", Default: true, Visibility: "all"}, {ID: 5, Name: "FireRunner", Visibility: "all", AllowsPublicRepositories: true}}
	f.groupRepos[5] = []int64{1, 2, 9}
	f.repos = []Repository{repo(1, "app", "private")}
	res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 5)
	if err != nil || res.State != "ready" || res.GroupID != 5 {
		t.Fatalf("result %+v, %v", res, err)
	}
	if g := f.groups[1]; g.Name != "firerunner" || g.Visibility != "selected" || g.AllowsPublicRepositories {
		t.Fatalf("group %+v", g)
	}
	if !slices.Equal(f.groupRepos[5], []int64{1}) {
		t.Fatalf("repositories %v", f.groupRepos[5])
	}
	if f.groups[0].Visibility != "all" {
		t.Fatal("the default group was changed")
	}

	// Right already: the repositories are set again, nothing else.
	f.requests = nil
	if _, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 5); err != nil {
		t.Fatal(err)
	}
	if w := writes(f); !slices.Equal(w, []string{"PUT /orgs/acme/actions/runner-groups/5/repositories"}) {
		t.Fatalf("writes %v", w)
	}
}

// The organization's default group is never onboarding's, even when its
// name is the one configured.
func TestSyncGroupNeverTheDefault(t *testing.T) {
	f, gh := newAPI(t)
	f.groups = []RunnerGroup{{ID: 1, Name: "firerunner", Default: true, Visibility: "all"}}
	f.repos = []Repository{repo(1, "app", "private")}
	for _, owned := range []int64{0, 1} {
		res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", owned)
		if err != nil || res.State != "unavailable" || !strings.Contains(res.Reason, "did not create") {
			t.Fatalf("owned %d: result %+v, %v", owned, res, err)
		}
	}
	if w := writes(f); len(w) != 0 {
		t.Fatalf("writes %v", w)
	}
}

// GitHub refusing the group (a plan without runner groups, a permission
// withdrawn) is a result to show; anything else is an error to retry.
func TestSyncGroupRefused(t *testing.T) {
	for name, c := range map[string]struct {
		code      int
		rateLimit bool
		wantErr   bool
	}{
		"forbidden":     {code: 403},
		"unprocessable": {code: 422},
		"not found":     {code: 404},
		"rate limited":  {code: 403, rateLimit: true, wantErr: true},
		"server error":  {code: 502, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			f, gh := newAPI(t)
			f.repos = []Repository{repo(1, "app", "private")}
			f.refuse["POST /orgs/acme/actions/runner-groups"] = c.code
			var g Groups = gh
			if c.rateLimit {
				g = rateLimited{gh}
			}
			res, err := SyncGroup(context.Background(), g, 7, "acme", "firerunner", 0)
			if c.wantErr {
				if err == nil {
					t.Fatalf("no error: %+v", res)
				}
				return
			}
			if err != nil || res.State != "unavailable" || !strings.Contains(res.Reason, "refused by the fake") {
				t.Fatalf("result %+v, %v", res, err)
			}
		})
	}
}

// rateLimited reports a create refused for a spent rate limit, which
// shares 403 with a refusal.
type rateLimited struct{ *GitHub }

func (r rateLimited) CreateRunnerGroup(context.Context, int64, string, GroupSpec) (RunnerGroup, error) {
	return RunnerGroup{}, &StatusError{Method: "POST", Path: "/orgs/acme/actions/runner-groups", Code: 403, RateLimited: true}
}

// groupStore is the installations' rows and their recorded results.
type groupStore struct {
	rows    map[int64]jobstore.Installation
	results map[int64]GroupResult
	owned   map[int64]int64
	locked  map[int64]bool
	fail    error
}

func (s *groupStore) ReadInstallation(_ context.Context, id int64) (jobstore.Installation, bool, error) {
	in, ok := s.rows[id]
	return in, ok, s.fail
}

func (s *groupStore) ActiveInstallations(context.Context) ([]jobstore.Installation, error) {
	var out []jobstore.Installation
	for _, in := range s.rows {
		if in.State == "active" {
			out = append(out, in)
		}
	}
	slices.SortFunc(out, func(a, b jobstore.Installation) int { return int(a.ID - b.ID) })
	return out, nil
}

func (s *groupStore) LockInstallation(_ context.Context, id int64) (func(), error) {
	if s.locked == nil {
		s.locked = map[int64]bool{}
	}
	if s.locked[id] {
		return nil, errors.New("locked twice")
	}
	s.locked[id] = true
	return func() { s.locked[id] = false }, nil
}

func (s *groupStore) OwnedGroup(_ context.Context, id int64) (int64, error) {
	return s.owned[id], nil
}

func (s *groupStore) RecordGroup(_ context.Context, id, group int64, b json.RawMessage) error {
	if s.owned == nil {
		s.owned = map[int64]int64{}
	}
	if group != 0 {
		s.owned[id] = group
	}
	var r GroupResult
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	s.results[id] = r
	return nil
}

// Keep keeps an active installation's group, under its organization's
// login now, and records the result; not one that is not active.
func TestKeeper(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	f.installations[7] = `{"id":7,"account":{"id":900,"login":"acme","type":"Organization"}}`
	f.installations[8] = `{"id":8,"account":{"id":901,"login":"beta","type":"Organization"},"suspended_at":"2026-11-18T10:00:00Z"}`
	store := &groupStore{results: map[int64]GroupResult{}, rows: map[int64]jobstore.Installation{
		7: {ID: 7, AccountID: 900, AccountLogin: "acme-old-name", State: "active"},
		8: {ID: 8, AccountID: 901, AccountLogin: "beta", State: "active"},
		9: {ID: 9, AccountID: 902, AccountLogin: "gamma", State: "suspended"},
	}}
	k := Keeper{Store: store, GitHub: gh, Lookup: gh.Installation, Group: "firerunner"}
	if err := k.KeepAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, ok := store.results[7]; !ok || r.State != "ready" || r.GroupID != 100 {
		t.Fatalf("installation 7: %+v", store.results)
	}
	if _, ok := store.results[8]; ok {
		t.Fatal("a group kept for an installation GitHub reports suspended")
	}
	if _, kept, err := k.Keep(context.Background(), 9); kept || err != nil {
		t.Fatalf("a suspended installation's group kept: %v %v", kept, err)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "acme-old-name") {
			t.Fatalf("the old login was used: %s", r)
		}
	}
}

// KeepAll goes on past an installation that fails, and says so.
func TestKeepAllGoesOn(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	store := &groupStore{results: map[int64]GroupResult{}, rows: map[int64]jobstore.Installation{
		6: {ID: 6, AccountID: 899, AccountLogin: "broken", State: "active"},
		7: {ID: 7, AccountID: 900, AccountLogin: "acme", State: "active"},
	}}
	f.refuse["GET /orgs/broken/actions/runner-groups"] = 502
	err := Keeper{Store: store, GitHub: gh, Group: "firerunner"}.KeepAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err %v", err)
	}
	if _, ok := store.results[7]; !ok {
		t.Fatal("a failure stopped the rest")
	}
	store.fail = errors.New("db down")
	if _, _, err := (Keeper{Store: store, GitHub: gh, Group: "firerunner"}).Keep(context.Background(), 7); err == nil {
		t.Fatal("a store failure was no error")
	}
	if _, _, err := (Keeper{Store: store, GitHub: gh}).Keep(context.Background(), 7); err == nil {
		t.Fatal("no group name was no error")
	}
}

// A delivery says when the installation's group is to be kept: listed, or
// its repositories changed; not suspended or refused.
func TestKeepGroupOutcome(t *testing.T) {
	s := newFake()
	if out := handle(t, s, event("created", 90, "acme", "", 0)); !out.KeepGroup {
		t.Fatalf("listed: %+v", out)
	}
	repos := sign(Delivery{ID: "r1", Event: "installation_repositories", Body: []byte(`{"action":"added","installation":{"id":90}}`)})
	if out := handle(t, s, repos); !out.KeepGroup || out.Installation != 90 {
		t.Fatalf("repositories changed: %+v", out)
	}
	if out := handle(t, s, event("suspend", 90, "acme", "", 1)); out.KeepGroup {
		t.Fatalf("suspended: %+v", out)
	}
	if out := handle(t, s, event("created", 91, "someone", "User", 0)); out.KeepGroup {
		t.Fatalf("refused: %+v", out)
	}
	bad := sign(Delivery{ID: "r2", Event: "installation_repositories", Body: []byte(`{"action":"added"}`)})
	if _, err := Handle(context.Background(), cfg, s, bad); err == nil {
		t.Fatal("no installation id was no error")
	}
}

// A group of the name that onboarding did not create is the
// organization's own: never adopted, never changed.
func TestSyncGroupNotOurs(t *testing.T) {
	f, gh := newAPI(t)
	f.groups = []RunnerGroup{{ID: 5, Name: "FireRunner", Visibility: "all"}}
	f.groupRepos[5] = []int64{42}
	f.repos = []Repository{repo(1, "app", "private")}
	res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 0)
	if err != nil || res.State != "unavailable" || !strings.Contains(res.Reason, "did not create") {
		t.Fatalf("result %+v, %v", res, err)
	}
	if w := writes(f); len(w) != 0 || f.groups[0].Visibility != "all" {
		t.Fatalf("writes %v, group %+v", w, f.groups[0])
	}
}

// Onboarding's group deleted by the organization is created again; one
// renamed while another group took its name is left, and says why.
func TestSyncGroupOwnedGone(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 77)
	if err != nil || res.State != "ready" || res.GroupID != 100 || len(f.groups) != 1 {
		t.Fatalf("deleted: result %+v, %v, groups %+v", res, err, f.groups)
	}

	f, gh = newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	f.groups = []RunnerGroup{{ID: 5, Name: "ours-renamed", Visibility: "selected"}, {ID: 6, Name: "firerunner", Visibility: "all"}}
	res, err = SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 5)
	if err != nil || res.State != "unavailable" || !strings.Contains(res.Reason, "renamed") {
		t.Fatalf("renamed: result %+v, %v", res, err)
	}
	if w := writes(f); len(w) != 0 {
		t.Fatalf("writes %v", w)
	}
}

// An installation that has not granted the runner permission cannot get
// a token for it: shown, not retried for ever.
func TestSyncGroupNoPermission(t *testing.T) {
	f, gh := newAPI(t)
	f.refuse["POST /app/installations/7/access_tokens"] = 422
	res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 0)
	if err != nil || res.State != "unavailable" || !strings.Contains(res.Reason, "permission") {
		t.Fatalf("result %+v, %v", res, err)
	}
}

// A secondary rate limit answers 403 with Retry-After: retried, not shown
// as a refusal.
func TestSyncGroupSecondaryRateLimit(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	f.refuse["POST /orgs/acme/actions/runner-groups"] = 403
	f.refuseHeader["Retry-After"] = "60"
	if res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 0); err == nil {
		t.Fatalf("no error: %+v", res)
	}
}

// A repository deleted between the listing and the setting is a failure to
// retry, which lists them again; not a refusal.
func TestSyncGroupRepositoryGone(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	f.groups = []RunnerGroup{{ID: 5, Name: "firerunner", Visibility: "selected"}}
	f.refuse["PUT /orgs/acme/actions/runner-groups/5/repositories"] = 422
	if res, err := SyncGroup(context.Background(), gh, 7, "acme", "firerunner", 5); err == nil {
		t.Fatalf("no error: %+v", res)
	}
}

// Keep records the group it created, keeps that one after, and holds the
// installation's lock throughout.
func TestKeeperOwnsWhatItCreates(t *testing.T) {
	f, gh := newAPI(t)
	f.repos = []Repository{repo(1, "app", "private")}
	store := &groupStore{results: map[int64]GroupResult{}, rows: map[int64]jobstore.Installation{
		7: {ID: 7, AccountID: 900, AccountLogin: "acme", State: "active"},
	}}
	k := Keeper{Store: store, GitHub: gh, Group: "firerunner"}
	for range 2 {
		if _, kept, err := k.Keep(context.Background(), 7); !kept || err != nil {
			t.Fatalf("kept %v, %v", kept, err)
		}
	}
	if store.owned[7] != 100 || len(f.groups) != 1 || store.results[7].State != "ready" {
		t.Fatalf("owned %v, groups %+v, result %+v", store.owned, f.groups, store.results[7])
	}
	if store.locked[7] {
		t.Fatal("the lock was not released")
	}
	store.locked[7] = true
	if _, _, err := k.Keep(context.Background(), 7); err == nil {
		t.Fatal("kept without the lock")
	}
}
