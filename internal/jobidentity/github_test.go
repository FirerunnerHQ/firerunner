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
	"os"
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
	runBases                                  map[int]string
	prs                                       map[int]map[string]any
	branchSHA, branchRef                      string
	tags                                      []map[string]any
	activity                                  [][]map[string]any // pages; nil = the run's own push
	compare                                   string
	failPath                                  string
	status                                    int
	corrupt                                   bool
}

func fixture() *apiFixture {
	return &apiFixture{event: "pull_request", head: "feature", headRepo: "fork/repo", sha: "abc123", defaultBranch: "main", candidates: []int{7}, runBases: map[int]string{7: "release"}, prs: map[int]map[string]any{7: prJSON(7, "release")}, branchSHA: "abc123", branchRef: "refs/heads/feature", compare: "identical"}
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
			candidates := []map[string]any{}
			for _, n := range f.candidates {
				candidates = append(candidates, map[string]any{"number": n, "base": map[string]any{"ref": f.runBases[n]}})
			}
			out = map[string]any{"id": 42, "event": f.event, "head_branch": f.head, "head_sha": f.sha, "repository": map[string]any{"full_name": "owner/repo"}, "head_repository": map[string]any{"full_name": f.headRepo}, "pull_requests": candidates}
		case r.URL.Path == "/repos/owner/repo":
			out = map[string]any{"full_name": "owner/repo", "default_branch": f.defaultBranch}
		case strings.Contains(r.URL.Path, "/commits/"):
			candidates := []map[string]any{}
			for _, p := range f.prs {
				candidates = append(candidates, p)
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
		case strings.HasSuffix(r.URL.Path, "/activity"):
			q := r.URL.Query()
			if q.Get("ref") != "refs/heads/"+f.head || q.Get("time_period") != "month" {
				t.Errorf("activity query %s", r.URL.RawQuery)
			}
			pages := f.activity
			if pages == nil {
				pages = [][]map[string]any{{{"ref": "refs/heads/" + f.head, "after": f.sha, "activity_type": "push"}}}
			}
			page := 0
			if after := q.Get("after"); after != "" {
				_, _ = fmt.Sscanf(after, "c%d", &page)
			}
			if page+1 < len(pages) {
				w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/repositories/1/activity?per_page=100&after=c%d>; rel="next"`, page+1))
			}
			out = []map[string]any{}
			if page < len(pages) {
				out = pages[page]
			}
		case strings.Contains(r.URL.Path, "/compare/"):
			if !strings.HasSuffix(r.URL.Path, "/compare/"+f.sha+"..."+f.head) {
				t.Errorf("compare path %s", r.URL.Path)
			}
			// status precedes commits and patches, which can exceed the 2 MiB limit.
			fmt.Fprintf(w, `{"url":"x","base_commit":{"sha":"b"},"status":%q,"files":[{"patch":"%s"}]}`, f.compare, strings.Repeat("x", 3<<20))
			return
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
		{"empty PR list fallback ignores live base", func(f *apiFixture) { f.candidates = nil }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"duplicate base/default", func(f *apiFixture) { f.prs[7] = prJSON(7, "main"); f.runBases[7] = "main" }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"retargeted from default", func(f *apiFixture) { f.runBases[7] = "main" }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"retargeted from another base", func(f *apiFixture) { f.runBases[7] = "old-release" }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"missing run base", func(f *apiFixture) { f.runBases = nil }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"invalid run base", func(f *apiFixture) { f.runBases[7] = "../release" }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
		{"base on another PR association", func(f *apiFixture) { f.runBases = map[int]string{8: "release"} }, "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}, false},
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
		{"deleted same-named tag push", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.activity = [][]map[string]any{}
			f.compare = "diverged"
		}, "", []string{"refs/heads/main"}, false},
		{"push record required even on branch history", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.activity = [][]map[string]any{}
			f.compare = "ahead"
		}, "", []string{"refs/heads/main"}, false},
		{"rerun after force push", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.branchSHA = "rewritten"
			f.compare = "diverged"
		}, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"merged PR record", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.activity = [][]map[string]any{{{"ref": "refs/heads/feature", "after": "abc123", "activity_type": "pr_merge"}}}
		}, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"tag activity is not a branch push", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.activity = [][]map[string]any{{{"ref": "refs/tags/feature", "after": "abc123", "activity_type": "push"}}}
		}, "", []string{"refs/heads/main"}, false},
		{"deletion record is not a push", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.activity = [][]map[string]any{{{"ref": "refs/heads/feature", "after": "abc123", "activity_type": "branch_deletion"}}}
		}, "", []string{"refs/heads/main"}, false},
		{"other commit pushed", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.activity = [][]map[string]any{{{"ref": "refs/heads/feature", "after": "other", "activity_type": "push"}}}
		}, "", []string{"refs/heads/main"}, false},
		{"GHES without activity uses ancestry", func(f *apiFixture) {
			f.branchSHA = "tip"
			f.event = "push"
			f.headRepo = "owner/repo"
			f.failPath = "/activity"
			f.status = 404
			f.compare = "ahead"
		}, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"GHES without activity, foreign commit", func(f *apiFixture) {
			f.branchSHA = "tip"
			f.event = "push"
			f.headRepo = "owner/repo"
			f.failPath = "/activity"
			f.status = 404
			f.compare = "diverged"
		}, "", []string{"refs/heads/main"}, false},
		{"activity error fails closed", func(f *apiFixture) {
			f.event = "push"
			f.headRepo = "owner/repo"
			f.failPath = "/activity"
			f.status = 500
		}, "", nil, true},
		{"dispatch on advanced branch", func(f *apiFixture) {
			f.branchSHA = "tip"
			f.event = "workflow_dispatch"
			f.headRepo = "owner/repo"
			f.compare = "ahead"
		}, "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}, false},
		{"dispatch on deleted same-named tag", func(f *apiFixture) {
			f.branchSHA = "tip"
			f.event = "workflow_dispatch"
			f.headRepo = "owner/repo"
			f.compare = "diverged"
		}, "", []string{"refs/heads/main"}, false},
		{"dispatch commit ahead of branch", func(f *apiFixture) {
			f.branchSHA = "tip"
			f.event = "workflow_dispatch"
			f.headRepo = "owner/repo"
			f.compare = "behind"
		}, "", []string{"refs/heads/main"}, false},
		{"compare error fails closed", func(f *apiFixture) {
			f.branchSHA = "tip"
			f.event = "workflow_dispatch"
			f.headRepo = "owner/repo"
			f.failPath = "/compare/"
			f.status = 500
		}, "", nil, true},
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
	// Avoid inheriting an override from the developer's environment.
	t.Setenv("GITHUB_ACTIONS_FORCE_GHES", "")
	if err := os.Unsetenv("GITHUB_ACTIONS_FORCE_GHES"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ raw, want string }{
		{"https://github.com/owner/repo", "https://api.github.com"},
		{"https://GitHub.COM/org", "https://api.github.com"},
		{"https://WWW.GitHub.com/org", "https://api.github.com"},
		{"https://tenant.ghe.com/org", "https://api.tenant.ghe.com"},
		{"https://Tenant.GHE.COM/org", "https://api.tenant.ghe.com"},
		{"https://github.localhost/org", "https://api.github.localhost"},
		{"https://ghe.example/org", "https://ghe.example/api/v3"},
		{"https://ghe.example:8443/org", "https://ghe.example:8443/api/v3"},
		{"https://ghe.com.evil.example/org", "https://ghe.com.evil.example/api/v3"},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			g, err := NewGitHub(tt.raw, "pat", scaleset.GitHubAppAuth{})
			if err != nil {
				t.Fatal(err)
			}
			if g.base != tt.want {
				t.Fatalf("base = %q, want %q", g.base, tt.want)
			}
		})
	}
	for _, value := range []string{"", "1", "false"} {
		t.Run("force GHES="+value, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS_FORCE_GHES", value)
			for _, host := range []string{"github.com", "tenant.ghe.com"} {
				g, err := NewGitHub("https://"+host+"/org", "pat", scaleset.GitHubAppAuth{})
				if err != nil {
					t.Fatal(err)
				}
				if g.base != "https://"+host+"/api/v3" {
					t.Fatalf("override ignored: %s", g.base)
				}
			}
		})
	}
	for _, raw := range []string{"http://github.com/owner", "https:///owner", "://invalid"} {
		if _, err := NewGitHub(raw, "pat", scaleset.GitHubAppAuth{}); err == nil {
			t.Fatalf("invalid URL accepted: %s", raw)
		}
	}
}

func TestRetargetedPRMemoization(t *testing.T) {
	f := fixture()
	f.runBases[7] = "main"
	srv := f.server(t)
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	for n := range 3 {
		if n == 2 {
			g.resolveMu.Lock()
			for key, result := range g.runs {
				result.expiry = time.Now().Add(-time.Second)
				g.runs[key] = result
			}
			g.resolveMu.Unlock()
		}
		id := fmt.Sprint(n)
		i, err := g.Resolve(context.Background(), Job{Repository: "owner/repo", RunID: 42, JobID: id, Event: f.event})
		if err != nil {
			t.Fatal(err)
		}
		if i.JobID != id || i.WriteRef != "refs/pull/7/merge" || !reflect.DeepEqual(i.ReadRefs, []string{"refs/pull/7/merge", "refs/heads/main"}) {
			t.Fatalf("retargeted PR gained base rights on lookup %d: %+v", n, i)
		}
		wantCalls := int32(3)
		if n == 2 {
			wantCalls = 6
		}
		if f.calls.Load() != wantCalls {
			t.Fatalf("requests = %d, want %d", f.calls.Load(), wantCalls)
		}
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

func TestPushRecordPagination(t *testing.T) {
	filler := func(n int) []map[string]any {
		var page []map[string]any
		for k := 0; k < n; k++ {
			page = append(page, map[string]any{"ref": "refs/heads/feature", "after": fmt.Sprintf("old%d", k), "activity_type": "push"})
		}
		return page
	}
	for _, tt := range []struct {
		name      string
		pages     int
		foundOn   int  // -1: never
		wantErr   bool // true: unproven, read-only identity
		wantPages int
	}{
		{"found on third page", 5, 2, false, 3},
		{"absent after the last page", 3, -1, true, 3},
		{"search is bounded", activityPages + 5, -1, true, activityPages},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := fixture()
			f.event, f.headRepo = "push", "owner/repo"
			for p := 0; p < tt.pages; p++ {
				page := filler(100)
				if p == tt.foundOn {
					page[57] = map[string]any{"ref": "refs/heads/feature", "after": f.sha, "activity_type": "push"}
				}
				f.activity = append(f.activity, page)
			}
			var activityCalls atomic.Int32
			f.requestURI = func(uri string) {
				if strings.Contains(uri, "/activity?") {
					activityCalls.Add(1)
				}
			}
			srv := f.server(t)
			defer srv.Close()
			g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
			i, err := g.Resolve(context.Background(), Job{Repository: "owner/repo", RunID: 42, JobID: "job", Event: "push"})
			wantWrite, wantReads := "refs/heads/feature", []string{"refs/heads/feature", "refs/heads/main"}
			if tt.wantErr { // unproven: no write, default-only reads
				wantWrite, wantReads = "", []string{"refs/heads/main"}
			}
			if err != nil || i.WriteRef != wantWrite || !reflect.DeepEqual(i.ReadRefs, wantReads) {
				t.Fatalf("identity %+v, err %v", i, err)
			}
			if int(activityCalls.Load()) != tt.wantPages {
				t.Fatalf("activity pages = %d, want %d", activityCalls.Load(), tt.wantPages)
			}
		})
	}
}

func TestNextCursor(t *testing.T) {
	link := `<https://api.github.com/repositories/1/activity?after=prev>; rel="prev", <https://api.github.com/repositories/1/activity?per_page=100&after=Y3Vy%3D>; rel="next"`
	if got := nextCursor(link); got != "Y3Vy=" {
		t.Fatalf("cursor %q", got)
	}
	if nextCursor(`<https://x/y?after=a>; rel="prev"`) != "" || nextCursor("") != "" {
		t.Fatal("cursor without next")
	}
}
