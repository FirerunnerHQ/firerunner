package jobidentity

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// finderFunc answers RunnerJob from a map of "repository#run" to the runner
// GitHub records on an in-progress job of that run, and counts the calls.
type fakeFinder struct {
	mu    sync.Mutex
	on    map[string]string // repository#run -> runner
	calls int
}

func (f *fakeFinder) RunnerJob(_ context.Context, repo string, run int64, runner string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.on[fmt.Sprintf("%s#%d", repo, run)] == runner {
		return run * 10, true, nil
	}
	return 0, false, nil
}

func (f *fakeFinder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func discoverRegistry(t *testing.T) (*Registry, Reader) {
	t.Helper()
	dir := privateDir(t)
	r, err := NewRegistry(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.discoverPause = time.Millisecond
	return r, Reader{Dir: dir}
}

// TestDiscoverFindsTheRunnersJobAmongTheOffered: the instance says its job
// started; GitHub's record of which job of which offered run is on its runner
// names the job, and the identity resolves from it, before any JobStarted.
func TestDiscoverFindsTheRunnersJobAmongTheOffered(t *testing.T) {
	r, reader := discoverRegistry(t)
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	cands := &Candidates{}
	cands.Offer(Job{Repository: "owner/other", RunID: 7, Event: "push"})
	cands.Offer(Job{Repository: "owner/repo", RunID: 42, Event: "pull_request", WorkflowRef: "owner/repo/.github/workflows/ci.yml@refs/pull/1/merge"})
	finder := &fakeFinder{on: map[string]string{"owner/repo#42": "runner-1"}}
	var got Job
	r.Discover(context.Background(), "runner-1", cands, finder, resolveFunc(func(_ context.Context, j Job) (Identity, error) {
		got = j
		return identity(), nil
	}))
	awaitIdentity(t, reader, "172.16.0.2")
	if got.Repository != "owner/repo" || got.RunID != 42 || got.Event != "pull_request" || got.JobID != "420" || got.Runner != "runner-1" {
		t.Fatalf("resolved %+v, want the offered run with GitHub's job on runner-1", got)
	}
}

// TestDiscoverWithoutAMatchLeavesItToJobStarted: a run named by no offer, or
// a job on another runner, proves nothing; JobStarted still resolves it.
func TestDiscoverWithoutAMatchLeavesItToJobStarted(t *testing.T) {
	r, reader := discoverRegistry(t)
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	cands := &Candidates{}
	cands.Offer(Job{Repository: "owner/repo", RunID: 42, Event: "push"})
	finder := &fakeFinder{on: map[string]string{"owner/repo#42": "runner-2"}}
	resolved := false
	r.Discover(context.Background(), "runner-1", cands, finder, resolveFunc(func(context.Context, Job) (Identity, error) {
		resolved = true
		return identity(), nil
	}))
	if resolved || finder.count() != discoverTries {
		t.Fatalf("resolved=%v after %d lookups, want none resolved after %d tries", resolved, finder.count(), discoverTries)
	}
	if _, err := reader.Lookup("172.16.0.2"); err == nil {
		t.Fatal("an identity without a match")
	}
	r.Start(context.Background(), Job{Runner: "runner-1", Repository: "owner/repo", RunID: 42, JobID: "j"},
		resolveFunc(func(context.Context, Job) (Identity, error) { return identity(), nil }))
	awaitIdentity(t, reader, "172.16.0.2")
}

func TestDiscoverLeavesAResolvingOrUnknownRunnerAlone(t *testing.T) {
	r, _ := discoverRegistry(t)
	cands := &Candidates{}
	cands.Offer(Job{Repository: "owner/repo", RunID: 42, Event: "push"})
	finder := &fakeFinder{on: map[string]string{"owner/repo#42": "runner-1"}}
	never := resolveFunc(func(context.Context, Job) (Identity, error) {
		t.Error("resolved a runner Discover should have left alone")
		return Identity{}, nil
	})
	r.Discover(context.Background(), "runner-1", cands, finder, never) // no lease
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	defer close(block)
	r.Start(context.Background(), Job{Runner: "runner-1", Repository: "owner/repo", RunID: 42, JobID: "j"},
		resolveFunc(func(ctx context.Context, _ Job) (Identity, error) {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return identity(), nil
		}))
	r.Discover(context.Background(), "runner-1", cands, finder, never) // JobStarted got there first
	if finder.count() != 0 {
		t.Fatalf("%d lookups, want none", finder.count())
	}
}

func TestCandidatesExpire(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := &Candidates{now: func() time.Time { return now }}
	c.Offer(Job{Repository: "owner/old", RunID: 1})
	// Literal times, not candidateTTL: a run offered half an hour ago is
	// stale, one offered 29 minutes ago is not.
	now = now.Add(29 * time.Minute)
	c.Offer(Job{Repository: "owner/new", RunID: 2})
	if got := c.recent(); len(got) != 2 {
		t.Fatalf("within the TTL: %d candidates, want both", len(got))
	}
	now = now.Add(2 * time.Minute)
	if got := c.recent(); len(got) != 1 || got[0].Repository != "owner/new" {
		t.Fatalf("past the first's TTL: %+v, want only the newer run", got)
	}
}

func TestCandidatesExpireAndAreBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := &Candidates{now: func() time.Time { return now }}
	c.Offer(Job{Repository: "owner/old", RunID: 1})
	now = now.Add(candidateTTL + time.Minute)
	for i := range maxCandidates + 5 {
		c.Offer(Job{Repository: "owner/repo", RunID: int64(i + 2)})
		now = now.Add(time.Second)
	}
	c.Offer(Job{Repository: "bad repo", RunID: 3})
	c.Offer(Job{Repository: "owner/repo", RunID: 0})
	got := c.recent()
	if len(got) != maxCandidates {
		t.Fatalf("%d candidates, want the newest %d", len(got), maxCandidates)
	}
	if got[0].RunID != maxCandidates+6 {
		t.Fatalf("first candidate run %d, want the newest", got[0].RunID)
	}
	for _, j := range got {
		if j.Repository == "owner/old" {
			t.Fatal("an expired run is still a candidate")
		}
	}
}

func TestGitHubRunnerJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/actions/runs/42/jobs" || r.URL.Query().Get("filter") != "latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"jobs":[
			{"id":1,"status":"completed","runner_name":"runner-1"},
			{"id":2,"status":"in_progress","runner_name":"runner-2"},
			{"id":3,"status":"in_progress","runner_name":"runner-1"}]}`)
	}))
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	if id, ok, err := g.RunnerJob(context.Background(), "owner/repo", 42, "runner-1"); err != nil || !ok || id != 3 {
		t.Fatalf("runner-1: %d %v %v, want its in-progress job 3", id, ok, err)
	}
	if _, ok, err := g.RunnerJob(context.Background(), "owner/repo", 42, "runner-9"); err != nil || ok {
		t.Fatalf("runner-9: %v %v, want no job", ok, err)
	}
	if _, _, err := g.RunnerJob(context.Background(), "owner/repo", 43, "runner-1"); err == nil {
		t.Fatal("a missing run is no error")
	}
	if _, _, err := g.RunnerJob(context.Background(), "../etc", 42, "runner-1"); err == nil {
		t.Fatal("an invalid repository was asked about")
	}
}

// TestDiscoverSkipsAnotherRunnersJob: a newer run whose in-progress job is on
// another runner is passed over for the older run that has this runner's.
func TestDiscoverSkipsAnotherRunnersJob(t *testing.T) {
	r, reader := discoverRegistry(t)
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	cands := &Candidates{now: func() time.Time { return now }}
	cands.Offer(Job{Repository: "owner/mine", RunID: 1, Event: "push"})
	now = now.Add(time.Second)
	cands.Offer(Job{Repository: "owner/theirs", RunID: 2, Event: "push"})
	finder := &fakeFinder{on: map[string]string{"owner/theirs#2": "runner-2", "owner/mine#1": "runner-1"}}
	var got Job
	r.Discover(context.Background(), "runner-1", cands, finder, resolveFunc(func(_ context.Context, j Job) (Identity, error) {
		got = j
		return identity(), nil
	}))
	awaitIdentity(t, reader, "172.16.0.2")
	if got.Repository != "owner/mine" {
		t.Fatalf("resolved %s, want the run with this runner's job", got.Repository)
	}
}

type limitedFinder struct{ calls int }

func (f *limitedFinder) RunnerJob(context.Context, string, int64, string) (int64, bool, error) {
	f.calls++
	return 0, false, statusError{"GET", "/repos/o/r/actions/runs/1/jobs", http.StatusForbidden}
}

// TestDiscoverStopsWhenGitHubRefuses: resolution shares the credential, so a
// rate limit ends the search at once instead of spending more of it.
func TestDiscoverStopsWhenGitHubRefuses(t *testing.T) {
	r, _ := discoverRegistry(t)
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	cands := &Candidates{}
	for i := range 5 {
		cands.Offer(Job{Repository: "owner/repo", RunID: int64(i + 1)})
	}
	f := &limitedFinder{}
	r.Discover(context.Background(), "runner-1", cands, f, resolveFunc(func(context.Context, Job) (Identity, error) { return identity(), nil }))
	if f.calls != 1 {
		t.Fatalf("%d lookups after a 403, want 1", f.calls)
	}
}

type refusingFinder struct{ calls int }

func (f *refusingFinder) RunnerJob(context.Context, string, int64, string) (int64, bool, error) {
	f.calls++
	return 0, false, fmt.Errorf("%w (owner/repo): HTTP 422", errTokenRefused)
}

// TestDiscoverWarnsOnceWhenGitHubRefusesAToken: on EC2 this is the path a job
// outside the App's installation takes, and JobStarted may come after the job
// ended, so Discover must say why at a level a default log keeps. It carries on,
// because another candidate may be the runner's.
func TestDiscoverWarnsOnceWhenGitHubRefusesAToken(t *testing.T) {
	var out bytes.Buffer
	r, err := NewRegistry(privateDir(t), slog.New(slog.NewTextHandler(&out, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.discoverPause = time.Millisecond
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	cands := &Candidates{}
	cands.Offer(Job{Repository: "owner/repo", RunID: 1})
	f := &refusingFinder{}
	r.Discover(context.Background(), "runner-1", cands, f, resolveFunc(func(context.Context, Job) (Identity, error) { return identity(), nil }))
	if f.calls != discoverTries {
		t.Fatalf("%d lookups, want %d", f.calls, discoverTries)
	}
	if n := strings.Count(out.String(), "is the App installed on it"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "level=WARN") {
		t.Fatalf("not a warning:\n%s", out.String())
	}
}

// TestStartRefusesADifferentJobForTheSameRunner: if Discover and JobStarted
// ever name different jobs for one runner, neither is trusted.
func TestStartRefusesADifferentJobForTheSameRunner(t *testing.T) {
	r, reader := discoverRegistry(t)
	if err := r.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	ok := resolveFunc(func(context.Context, Job) (Identity, error) { return identity(), nil })
	r.Start(context.Background(), Job{Runner: "runner-1", Repository: "owner/repo", RunID: 42, JobID: "j"}, ok)
	awaitIdentity(t, reader, "172.16.0.2")
	r.Start(context.Background(), Job{Runner: "runner-1", Repository: "owner/repo", RunID: 42, JobID: "j"}, ok) // the same: fine
	if _, err := reader.Lookup("172.16.0.2"); err != nil {
		t.Fatalf("the same job named twice: %v", err)
	}
	r.Start(context.Background(), Job{Runner: "runner-1", Repository: "owner/other", RunID: 7, JobID: "k"}, ok)
	if _, err := reader.Lookup("172.16.0.2"); err == nil {
		t.Fatal("still authorized after two sources named different jobs")
	}
}

// TestGitHubRunnerJobSharesAListingAndPages: a burst of lookups lists a run
// once, and a run of more than 100 jobs is read past its first page.
func TestGitHubRunnerJobSharesAListingAndPages(t *testing.T) {
	var lists, pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		page := r.URL.Query().Get("page")
		if page == "1" {
			lists.Add(1)
			time.Sleep(20 * time.Millisecond) // let the burst pile up behind it
		}
		var jobs []string
		switch page {
		case "1":
			for i := range 100 {
				jobs = append(jobs, fmt.Sprintf(`{"id":%d,"status":"in_progress","runner_name":"r-%d"}`, i, i))
			}
		case "2":
			jobs = append(jobs, `{"id":150,"status":"in_progress","runner_name":"late"}`)
		}
		fmt.Fprintf(w, `{"jobs":[%s]}`, strings.Join(jobs, ","))
	}))
	defer srv.Close()
	g := &GitHub{base: srv.URL, pat: "pat", client: srv.Client()}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, ok, err := g.RunnerJob(context.Background(), "owner/repo", 42, "late"); err != nil || !ok || id != 150 {
				t.Errorf("late: %d %v %v, want job 150 from page 2", id, ok, err)
			}
		}()
	}
	wg.Wait()
	if lists.Load() != 1 || pages.Load() != 2 {
		t.Fatalf("%d listings over %d pages for 10 lookups, want 1 listing of 2 pages", lists.Load(), pages.Load())
	}
}
