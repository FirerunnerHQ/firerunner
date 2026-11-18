package onboard

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/ghauth"
)

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
)

func testJWT(t *testing.T) *ghauth.JWT {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return ghauth.NewJWT("Iv1.test", ghauth.KeySigner{Key: key})
}

// fakeAPI is GitHub's REST API for one organization, as onboarding uses it.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	bodies   map[string]map[string]any
	tokens   int
	// what it holds
	installations map[int64]string // id -> response JSON; absent is 404
	repos         []Repository
	selection     string
	groups        []RunnerGroup
	groupRepos    map[int64][]int64
	refuse        map[string]int // "METHOD path" -> status
	refuseHeader  map[string]string
	// unauthorized, while > 0, answers that many installation calls 401.
	unauthorized int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, key+"?"+r.URL.RawQuery)
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			f.t.Errorf("%s: body %s", key, b)
		}
		f.bodies[key] = body
	}
	if code, ok := f.refuse[key]; ok {
		for k, v := range f.refuseHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"message":"refused by the fake"}`)
		return
	}
	auth := r.Header.Get("Authorization")
	app := strings.HasPrefix(r.URL.Path, "/app/")
	if app != !strings.HasPrefix(auth, "Bearer ghs_") {
		f.t.Errorf("%s with %q: the App's own calls take its JWT, the rest an installation token", key, auth)
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if !app && f.unauthorized > 0 {
		f.unauthorized--
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "%d", &id)
		resp, ok := f.installations[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, resp)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		f.tokens++
		write(map[string]any{"token": fmt.Sprintf("ghs_%d", f.tokens), "expires_at": time.Now().Add(time.Hour)})
	case key == "GET /installation/repositories":
		page := pageOf(r)
		write(map[string]any{"total_count": len(f.repos), "repository_selection": f.selection, "repositories": pageSlice(f.repos, page)})
	case key == "GET /orgs/acme/actions/runner-groups":
		write(map[string]any{"total_count": len(f.groups), "runner_groups": pageSlice(f.groups, pageOf(r))})
	case key == "POST /orgs/acme/actions/runner-groups":
		g := RunnerGroup{ID: int64(100 + len(f.groups)), Name: body["name"].(string), Visibility: body["visibility"].(string)}
		f.groups = append(f.groups, g)
		f.groupRepos[g.ID] = ids(body["selected_repository_ids"])
		w.WriteHeader(201)
		write(g)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/orgs/acme/actions/runner-groups/"):
		for i := range f.groups {
			if r.URL.Path == fmt.Sprintf("/orgs/acme/actions/runner-groups/%d", f.groups[i].ID) {
				f.groups[i].Name = body["name"].(string)
				f.groups[i].Visibility = body["visibility"].(string)
				f.groups[i].AllowsPublicRepositories = body["allows_public_repositories"].(bool)
			}
		}
		write(map[string]any{})
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/repositories"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/orgs/acme/actions/runner-groups/"), "%d", &id)
		f.groupRepos[id] = ids(body["selected_repository_ids"])
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected %s", key)
		w.WriteHeader(500)
	}
}

func pageOf(r *http.Request) int {
	var p int
	fmt.Sscanf(r.URL.Query().Get("page"), "%d", &p)
	if r.URL.Query().Get("per_page") != "100" || p < 1 {
		return -1
	}
	return p
}

func pageSlice[T any](all []T, page int) []T {
	if page < 1 {
		return nil
	}
	lo, hi := (page-1)*100, page*100
	lo, hi = min(lo, len(all)), min(hi, len(all))
	return all[lo:hi]
}

func ids(v any) []int64 {
	if v == nil {
		return nil
	}
	out := []int64{}
	for _, x := range v.([]any) {
		out = append(out, int64(x.(float64)))
	}
	return out
}

func newAPI(t *testing.T) (*fakeAPI, *GitHub) {
	f := &fakeAPI{t: t, bodies: map[string]map[string]any{}, installations: map[int64]string{}, groupRepos: map[int64][]int64{}, refuse: map[string]int{}, refuseHeader: map[string]string{}, selection: "selected"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewGitHub(srv.URL, testJWT(t))
}

// Installation reads an installation as the App: found, suspended, or gone.
func TestGitHubInstallation(t *testing.T) {
	f, gh := newAPI(t)
	f.installations[7] = `{"id":7,"account":{"id":900,"login":"acme","type":"Organization"},"suspended_at":null}`
	f.installations[8] = `{"id":8,"account":{"id":901,"login":"beta","type":"Organization"},"suspended_at":"2026-11-18T10:00:00Z"}`
	ctx := context.Background()
	if o, err := gh.Installation(ctx, 7); err != nil || o != (Observed{Found: true, AccountID: 900, Login: "acme", Type: "Organization"}) {
		t.Fatalf("7: %+v %v", o, err)
	}
	if o, err := gh.Installation(ctx, 8); err != nil || !o.Found || !o.Suspended {
		t.Fatalf("8: %+v %v", o, err)
	}
	if o, err := gh.Installation(ctx, 9); err != nil || o.Found {
		t.Fatalf("9, gone: %+v %v", o, err)
	}
	f.refuse["GET /app/installations/10"] = 502
	if _, err := gh.Installation(ctx, 10); statusOf(err) != 502 {
		t.Fatalf("a GitHub error is an error to retry: %v", err)
	}
}

// The installation token asks for what keeping a runner group needs alone,
// and is reused; repositories are read every page.
func TestGitHubRepositoriesAndToken(t *testing.T) {
	f, gh := newAPI(t)
	for i := range 230 {
		f.repos = append(f.repos, Repository{ID: int64(i + 1), FullName: fmt.Sprintf("acme/r%d", i), Private: true, Visibility: "private"})
	}
	ctx := context.Background()
	repos, all, err := gh.Repositories(ctx, 7)
	if err != nil || len(repos) != 230 || all {
		t.Fatalf("%d repos, all %v, %v", len(repos), all, err)
	}
	if _, err := gh.RunnerGroups(ctx, 7, "acme"); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 1 {
		t.Fatalf("%d tokens minted, want 1 reused", f.tokens)
	}
	perms := f.bodies["POST /app/installations/7/access_tokens"]["permissions"].(map[string]any)
	if len(perms) != 2 || perms["organization_self_hosted_runners"] != "write" || perms["metadata"] != "read" {
		t.Fatalf("token permissions %v", perms)
	}
}

// Create and update keep public repositories out, always.
func TestGitHubGroupWrites(t *testing.T) {
	f, gh := newAPI(t)
	ctx := context.Background()
	g, err := gh.CreateRunnerGroup(ctx, 7, "acme", GroupSpec{Name: "firerunner", Visibility: "selected", Repositories: []int64{3, 4}})
	if err != nil || g.ID != 100 {
		t.Fatalf("%+v %v", g, err)
	}
	body := f.bodies["POST /orgs/acme/actions/runner-groups"]
	if body["allows_public_repositories"] != false || body["visibility"] != "selected" || !slices.Equal(ids(body["selected_repository_ids"]), []int64{3, 4}) {
		t.Fatalf("create body %v", body)
	}
	if _, err := gh.CreateRunnerGroup(ctx, 7, "acme", GroupSpec{Name: "other", Visibility: "selected"}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies["POST /orgs/acme/actions/runner-groups"]["selected_repository_ids"]; got == nil {
		t.Fatal("an empty selection was sent as null, not []")
	}
	if err := gh.UpdateRunnerGroup(ctx, 7, "acme", 100, GroupSpec{Name: "firerunner", Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	if body := f.bodies["PATCH /orgs/acme/actions/runner-groups/100"]; body["allows_public_repositories"] != false || body["visibility"] != "private" {
		t.Fatalf("update body %v", body)
	}
	f.refuse["PUT /orgs/acme/actions/runner-groups/100/repositories"] = 403
	err = gh.SetGroupRepositories(ctx, 7, "acme", 100, nil)
	if statusOf(err) != 403 || !strings.Contains(err.Error(), "refused by the fake") {
		t.Fatalf("a refusal: %v", err)
	}
}

// A token GitHub no longer takes is forgotten, and the next call mints one.
func TestGitHubTokenRefused(t *testing.T) {
	f, gh := newAPI(t)
	ctx := context.Background()
	if _, err := gh.RunnerGroups(ctx, 7, "acme"); err != nil {
		t.Fatal(err)
	}
	f.unauthorized = 1
	if _, err := gh.RunnerGroups(ctx, 7, "acme"); statusOf(err) != 401 {
		t.Fatalf("err %v", err)
	}
	if _, err := gh.RunnerGroups(ctx, 7, "acme"); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 {
		t.Fatalf("%d tokens minted, want a second after the 401", f.tokens)
	}
}

// Every page of groups is read.
func TestGitHubRunnerGroupsPages(t *testing.T) {
	f, gh := newAPI(t)
	for i := range 150 {
		f.groups = append(f.groups, RunnerGroup{ID: int64(i + 1), Name: fmt.Sprintf("g%d", i)})
	}
	gs, err := gh.RunnerGroups(context.Background(), 7, "acme")
	if err != nil || len(gs) != 150 {
		t.Fatalf("%d groups, %v", len(gs), err)
	}
}

// Rate limits and GitHub's own failures are to retry; other refusals not.
func TestStatusErrorRetryable(t *testing.T) {
	for _, c := range []struct {
		e    StatusError
		want bool
	}{
		{StatusError{Code: 403}, false},
		{StatusError{Code: 403, RateLimited: true}, true},
		{StatusError{Code: 403, RetryAfter: true}, true},
		{StatusError{Code: 429}, true},
		{StatusError{Code: 502}, true},
		{StatusError{Code: 422}, false},
	} {
		if got := c.e.Retryable(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.e, got, c.want)
		}
	}
}
