package jobidentity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v4"
)

type apiFixture struct {
	calls                                     atomic.Int32
	requestURI                                func(string)
	event, head, headRepo, sha, defaultBranch string
	candidates                                []int
	prs                                       map[int]map[string]any
	branchSHA, branchRef                      string
	tags                                      []map[string]any
	failPath                                  string
	status                                    int
	corrupt                                   bool
}

func fixture() *apiFixture {
	return &apiFixture{event: "pull_request", head: "feature", headRepo: "fork/repo", sha: "abc123", defaultBranch: "main", candidates: []int{7}, prs: map[int]map[string]any{7: prJSON(7, "release")}, branchSHA: "abc123", branchRef: "refs/heads/feature"}
}
func prJSON(n int, base string) map[string]any {
	return map[string]any{"number": n, "head": map[string]any{"ref": "feature", "sha": "abc123", "repo": map[string]any{"full_name": "fork/repo"}}, "base": map[string]any{"ref": base, "repo": map[string]any{"full_name": "owner/repo"}}}
}
func (f *apiFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.requestURI != nil {
			f.requestURI(r.RequestURI)
		}
		if r.Header.Get("Authorization") != "Bearer pat" {
			t.Errorf("wrong auth")
		}
		if r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("API version missing")
		}
		if f.failPath != "" && strings.Contains(r.URL.Path, f.failPath) {
			w.WriteHeader(f.status)
			if f.corrupt {
				fmt.Fprint(w, "{")
			}
			return
		}
		var out any
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs/42"):
			candidates := []map[string]int{}
			for _, n := range f.candidates {
				candidates = append(candidates, map[string]int{"number": n})
			}
			out = map[string]any{"id": 42, "event": f.event, "head_branch": f.head, "head_sha": f.sha, "repository": map[string]any{"full_name": "owner/repo"}, "head_repository": map[string]any{"full_name": f.headRepo}, "pull_requests": candidates}
		case r.URL.Path == "/repos/owner/repo":
			out = map[string]any{"full_name": "owner/repo", "default_branch": f.defaultBranch}
		case strings.Contains(r.URL.Path, "/commits/"):
			candidates := []map[string]int{}
			for n := range f.prs {
				candidates = append(candidates, map[string]int{"number": n})
			}
			out = candidates
		case strings.Contains(r.URL.Path, "/pulls/"):
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/owner/repo/pulls/%d", &n)
			out = f.prs[n]
		case strings.Contains(r.URL.Path, "/git/matching-refs/tags/"):
			out = f.tags
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			out = map[string]any{"ref": f.branchRef, "object": map[string]any{"sha": f.branchSHA}}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func TestResolveGitHubPolicy(t *testing.T) {
	cases := []struct {
		name      string
		change    func(*apiFixture)
		wantWrite string
		wantReads []string
		wantErr   bool
	}{
		{"fork PR", func(*apiFixture) {}, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/release", "refs/heads/main"}, false},
		{"empty PR list fallback", func(f *apiFixture) { f.candidates = nil }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/release", "refs/heads/main"}, false},
		{"duplicate base/default", func(f *apiFixture) { f.prs[7] = prJSON(7, "main") }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"missing PR", func(f *apiFixture) { f.candidates = nil; f.prs = nil }, "", nil, true},
		{"ambiguous PR", func(f *apiFixture) { f.candidates = []int{7, 8}; f.prs[8] = prJSON(8, "other") }, "", nil, true},
		{"wrong head repo", func(f *apiFixture) { f.headRepo = "attacker/repo" }, "", nil, true},
		{"head advanced", func(f *apiFixture) { f.sha = "old" }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/release", "refs/heads/main"}, false},
		{"wrong base repo", func(f *apiFixture) {
			f.prs[7]["base"] = map[string]any{"ref": "main", "repo": map[string]any{"full_name": "attacker/repo"}}
		}, "", nil, true},
		{"invalid default", func(f *apiFixture) { f.defaultBranch = "" }, "", nil, true},
		{"invalid base", func(f *apiFixture) { f.prs[7] = prJSON(7, "") }, "", nil, true},
		{"push", func(f *apiFixture) { f.event = "push"; f.headRepo = "owner/repo" }, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"schedule", func(f *apiFixture) {
			f.event = "schedule"
			f.headRepo = "owner/repo"
			f.head = "main"
			f.branchRef = "refs/heads/main"
		}, "refs/heads/main", []string{"refs/heads/main"}, false},
		{"manual", func(f *apiFixture) { f.event = "workflow_dispatch"; f.headRepo = "owner/repo" }, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"repository dispatch", func(f *apiFixture) { f.event = "repository_dispatch"; f.headRepo = "owner/repo" }, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"tag name collision", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.tags = []map[string]any{{"ref": "refs/tags/feature"}}
		}, "", nil, true},
		{"other tag does not collide", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.tags = []map[string]any{{"ref": "refs/tags/feature-extra"}}
		}, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"branch advanced", func(f *apiFixture) { f.event = "push"; f.headRepo = "owner/repo"; f.branchSHA = "newer" }, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"foreign push", func(f *apiFixture) { f.event = "push" }, "", nil, true},
		{"pull request target read only", func(f *apiFixture) { f.event = "pull_request_target" }, "", []string{"refs/heads/main"}, false},
		{"workflow run read only", func(f *apiFixture) { f.event = "workflow_run" }, "", []string{"refs/heads/main"}, false},
		{"unknown event read only", func(f *apiFixture) { f.event = "new_event" }, "", []string{"refs/heads/main"}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := fixture()
			tt.change(f)
			srv := f.server(t)
			defer srv.Close()
			g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
			j := Job{Repository: "owner/repo", RunID: 42, JobID: "job", Event: f.event}
			i, err := g.Resolve(context.Background(), j)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("granted ambiguous identity %+v", i)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if i.WriteRef != tt.wantWrite || !reflect.DeepEqual(i.ReadRefs, tt.wantReads) {
				t.Fatalf("policy %+v", i)
			}
			if i.RunID != 42 || i.JobID != "job" || i.Repository != "owner/repo" {
				t.Fatal("job provenance lost")
			}
			if i.Fork != (f.headRepo != "owner/repo") {
				t.Fatal("wrong fork flag")
			}
		})
	}
}

func TestGitHubFailuresFailClosed(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := fixture()
			f.failPath = "/actions/runs/"
			f.status = status
			f.corrupt = true
			srv := f.server(t)
			defer srv.Close()
			g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
			if _, err := g.Resolve(context.Background(), Job{Repository: "owner/repo", RunID: 42, JobID: "job", Event: f.event}); err == nil {
				t.Fatal("API failure authorized")
			}
		})
	}
	f := fixture()
	srv := f.server(t)
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	for _, j := range []Job{{Repository: "../repo", RunID: 42, JobID: "j"}, {Repository: "owner/repo", JobID: "j"}, {Repository: "owner/repo", RunID: 42, JobID: "j", Event: "push"}} {
		if _, err := g.Resolve(context.Background(), j); err == nil {
			t.Fatal("job mismatch authorized")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Resolve(ctx, Job{Repository: "owner/repo", RunID: 42, JobID: "job", Event: f.event}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestInstallationTokenRefresh(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/app/installations/7/access_tokens" {
			t.Error("wrong installation request")
		}
		token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
		if err != nil || !token.Valid {
			t.Errorf("invalid app JWT: %v", err)
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if (len(body.Repositories) != 1 || (body.Repositories[0] != "repo" && body.Repositories[0] != "other")) || !reflect.DeepEqual(body.Permissions, map[string]string{"actions": "read", "pull_requests": "read", "metadata": "read", "contents": "read"}) {
			t.Errorf("overbroad token: %+v", body)
		}
		n := calls.Add(1)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprint(n), "expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	g := &GitHub{base: srv.URL, client: srv.Client(), app: scaleset.GitHubAppAuth{ClientID: "client", InstallationID: 7, PrivateKey: string(raw)}}
	for range 2 {
		v, err := g.auth(context.Background(), "owner/repo")
		if err != nil || v != "1" {
			t.Fatalf("token cache: %s %v", v, err)
		}
	}
	g.tokens["owner/repo"] = installationToken{value: "old", expiry: time.Now()}
	v, err := g.auth(context.Background(), "owner/repo")
	if err != nil || v != "2" || calls.Load() != 2 {
		t.Fatalf("refresh: %s %v", v, err)
	}
	if v, err := g.auth(context.Background(), "owner/other"); err != nil || v != "3" {
		t.Fatalf("different repository reused token: %s %v", v, err)
	}
	if v, err := g.auth(context.Background(), "owner/repo"); err != nil || v != "2" || calls.Load() != 3 {
		t.Fatalf("repository token cache: %s %v", v, err)
	}
	g.tokens["owner/repo"] = installationToken{value: "old", expiry: time.Now()}
	g.app.PrivateKey = "invalid"
	if _, err := g.auth(context.Background(), "owner/repo"); err == nil {
		t.Fatal("invalid private key accepted")
	}
}

func TestGitHubURL(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{{"https://github.com/owner/repo", "https://api.github.com"}, {"https://ghe.example/org", "https://ghe.example/api/v3"}} {
		g, err := NewGitHub(tt.raw, "pat", scaleset.GitHubAppAuth{})
		if err != nil || g.base != tt.want {
			t.Fatalf("%v %v", g, err)
		}
	}
	if _, err := NewGitHub("http://github.com/owner", "pat", scaleset.GitHubAppAuth{}); err == nil {
		t.Fatal("insecure URL accepted")
	}
}

func TestRunMemoizationMatrixAndExpiry(t *testing.T) {
	f := fixture()
	srv := f.server(t)
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	var wg sync.WaitGroup
	for n := range 30 {
		wg.Go(func() {
			id := fmt.Sprint(n)
			i, err := g.Resolve(context.Background(), Job{Repository: "owner/repo", RunID: 42, JobID: id, Event: f.event})
			if err != nil || i.JobID != id || i.WriteRef != "refs/pull/7/merge" {
				t.Errorf("matrix identity %+v %v", i, err)
			}
			i.ReadRefs[0] = "mutated"
		})
	}
	wg.Wait()
	if f.calls.Load() != 3 {
		t.Fatalf("matrix made %d requests, want 3", f.calls.Load())
	}
	g.resolveMu.Lock()
	for k, v := range g.runs {
		v.expiry = time.Now().Add(-time.Second)
		g.runs[k] = v
	}
	g.resolveMu.Unlock()
	if _, err := g.Resolve(context.Background(), Job{Repository: "owner/repo", RunID: 42, JobID: "rerun", Event: f.event}); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 6 {
		t.Fatal("expired policy reused")
	}
}
func TestSlashedBranchAndReusableWorkflowDoNotChangeScope(t *testing.T) {
	f := fixture()
	f.event = "push"
	f.headRepo = "owner/repo"
	f.head = "feature/queued"
	f.branchRef = "refs/heads/feature/queued"
	f.branchSHA = "advanced"
	f.requestURI = func(uri string) {
		if strings.Contains(uri, "%2F") {
			t.Errorf("escaped branch slash: %s", uri)
		}
	}
	srv := f.server(t)
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	i, err := g.Resolve(context.Background(), Job{Repository: "owner/repo", RunID: 42, JobID: "job", Event: "push", WorkflowRef: "owner/repo/.github/workflows/reusable.yml@refs/heads/main"})
	if err != nil || i.WriteRef != f.branchRef {
		t.Fatalf("workflow ref elevated queued branch: %+v %v", i, err)
	}
}

func TestRunMemoDoesNotCrossRepositoryOrEvent(t *testing.T) {
	f := fixture()
	srv := f.server(t)
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	j := Job{Repository: "owner/repo", RunID: 42, JobID: "job", Event: f.event}
	if _, err := g.Resolve(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j.Repository = "owner/other"
	if _, err := g.Resolve(context.Background(), j); err == nil {
		t.Fatal("memo crossed repository")
	}
	j.Repository = "owner/repo"
	j.Event = "push"
	if _, err := g.Resolve(context.Background(), j); err == nil {
		t.Fatal("memo crossed event")
	}
}
