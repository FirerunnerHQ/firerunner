package jobidentity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
