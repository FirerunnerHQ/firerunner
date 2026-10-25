package listener

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestScalerHandleDesiredRunnerCount(t *testing.T) {
	var gotDesired int
	a := &scaler{
		log: testLogger(),
		onDesired: func(_ context.Context, desired int) int {
			gotDesired = desired
			return 3
		},
	}
	running, err := a.HandleDesiredRunnerCount(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if gotDesired != 4 {
		t.Fatalf("desired = %d, want GitHub's count (4): the scheduler adds its floor", gotDesired)
	}
	if running != 3 {
		t.Fatalf("running = %d, want 3 (value returned by onDesired)", running)
	}
}

func TestScalerAdvertisesCapacityAfterEveryDesiredCount(t *testing.T) {
	capacity := 5
	var advertised []int
	a := &scaler{
		maxRunners:  8,
		log:         testLogger(),
		onDesired:   func(context.Context, int) int { return 0 },
		capacity:    func() int { return capacity },
		setCapacity: func(c int) { advertised = append(advertised, c) },
	}
	for _, c := range []int{5, 2, 2, 7} {
		capacity = c
		if _, err := a.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	// Every callback re-reads capacity and pushes it, so the library's next
	// GetMessage always carries the current value, even when unchanged.
	want := []int{5, 2, 2, 7}
	if len(advertised) != len(want) {
		t.Fatalf("advertised %v, want %v", advertised, want)
	}
	for i := range want {
		if advertised[i] != want[i] {
			t.Fatalf("advertised %v, want %v", advertised, want)
		}
	}
}

func TestScalerClampsAdvertisedCapacity(t *testing.T) {
	cases := []struct {
		name     string
		capacity int
		want     int
	}{
		{"above tier max is capped", 50, 8},
		{"at tier max passes", 8, 8},
		{"zero becomes one", 0, 1},
		{"negative becomes one", -3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			a := &scaler{
				maxRunners:  8,
				log:         testLogger(),
				onDesired:   func(context.Context, int) int { return 0 },
				capacity:    func() int { return tc.capacity },
				setCapacity: func(c int) { got = c },
			}
			if _, err := a.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("advertised %d, want %d", got, tc.want)
			}
		})
	}
}

func TestScalerCapacityReadAfterDesired(t *testing.T) {
	// onDesired reconciles (launching VMs), so the capacity GitHub sees must be
	// sampled after it, not before — otherwise a burst would be advertised
	// against a stale, pre-launch count.
	var order []string
	a := &scaler{
		maxRunners: 4,
		log:        testLogger(),
		onDesired: func(context.Context, int) int {
			order = append(order, "desired")
			return 1
		},
		capacity:    func() int { order = append(order, "capacity"); return 1 },
		setCapacity: func(int) { order = append(order, "set") },
	}
	if _, err := a.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "desired" || order[1] != "capacity" || order[2] != "set" {
		t.Fatalf("call order = %v, want [desired capacity set]", order)
	}
}

func TestScalerWithoutCapacityFuncNeverAdvertises(t *testing.T) {
	a := &scaler{
		maxRunners:  4,
		log:         testLogger(),
		onDesired:   func(context.Context, int) int { return 0 },
		setCapacity: func(int) { t.Fatal("setCapacity must not be called without a CapacityFunc") },
	}
	if _, err := a.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// And a CapacityFunc with nowhere to send it is equally inert.
	b := &scaler{
		maxRunners: 4,
		log:        testLogger(),
		onDesired:  func(context.Context, int) int { return 0 },
		capacity:   func() int { t.Fatal("capacity must not be read without a sink"); return 0 },
	}
	if _, err := b.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestScalerJobCallbacks(t *testing.T) {
	var busy, completed []string
	a := &scaler{
		log:         testLogger(),
		onDesired:   func(context.Context, int) int { return 0 },
		onBusy:      func(name string) { busy = append(busy, name) },
		onCompleted: func(name string) { completed = append(completed, name) },
	}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatal(err)
	}
	if len(busy) != 1 || busy[0] != "r" {
		t.Fatalf("HandleJobStarted should mark runner busy: got %v", busy)
	}
	// A nameless JobStarted must not mark anything busy.
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{}); err != nil {
		t.Fatal(err)
	}
	if len(busy) != 1 {
		t.Fatalf("nameless JobStarted should not mark busy: got %v", busy)
	}
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "r", Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0] != "r" {
		t.Fatalf("HandleJobCompleted should report the runner's completion: got %v", completed)
	}
	// A nameless JobCompleted reports nothing.
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{}); err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 {
		t.Fatalf("nameless JobCompleted should report nothing: got %v", completed)
	}
	// Nor does a scaler without the callback panic.
	b := &scaler{log: testLogger(), onDesired: func(context.Context, int) int { return 0 }}
	if err := b.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "r"}); err != nil {
		t.Fatal(err)
	}
}

func TestScalerJobStartedNilBusyFunc(t *testing.T) {
	a := &scaler{log: testLogger(), onDesired: func(context.Context, int) int { return 0 }}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatalf("HandleJobStarted with nil onBusy must not panic: %v", err)
	}
}

func TestBuildLabels(t *testing.T) {
	if got := buildLabels("firerunner", nil); len(got) != 1 || got[0].Name != "firerunner" {
		t.Fatalf("default labels = %v, want [firerunner]", got)
	}
	// The name is always advertised first; extra labels are additive and the
	// name is not duplicated if repeated in the extras.
	got := buildLabels("firerunner-node", []string{"node", "firerunner-node", "gpu"})
	if len(got) != 3 || got[0].Name != "firerunner-node" || got[1].Name != "node" || got[2].Name != "gpu" {
		t.Fatalf("labels = %v, want [firerunner-node node gpu]", got)
	}
}

func TestSystemInfoDefaults(t *testing.T) {
	si := systemInfo(Config{}, 7)
	if si.System != "firerunner" || si.Subsystem != "listener" {
		t.Fatalf("system info = %+v", si)
	}
	if si.Version != "dev" || si.CommitSHA != "NA" || si.ScaleSetID != 7 {
		t.Fatalf("defaults not applied: %+v", si)
	}
}

func TestRandSuffixUnique(t *testing.T) {
	if a, b := randSuffix(), randSuffix(); a == b {
		t.Fatalf("randSuffix collided: %q", a)
	}
	if got := randSuffix(); len(got) != 8 {
		t.Fatalf("randSuffix len = %d, want 8", len(got))
	}
}

// Compile-time proof the JIT source satisfies scheduler's expectation.
var _ interface {
	Generate(context.Context, core.RunnerSpec) (string, string, error)
} = (*JITSource)(nil)

func TestIsSessionConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"conflict 409", errors.New(`unexpected status code 409 Conflict: RunnerScaleSetSessionConflictException`), true},
		{"conflict mixed case", errors.New("The scaleset already has an active session (CONFLICT)"), true},
		{"unrelated", errors.New("401 Unauthorized: bad credentials"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionConflict(tc.err); got != tc.want {
				t.Fatalf("isSessionConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type lookup struct {
	ref *scaleset.RunnerReference
	err error
}

// fakeRemover answers GetRunnerByName from lookups in order (the last one
// repeats) and RemoveRunner with removeErr.
type fakeRemover struct {
	lookups   []lookup
	removeErr error
	removed   []int64
	looked    int
}

func (f *fakeRemover) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	l := f.lookups[min(f.looked, len(f.lookups)-1)]
	f.looked++
	return l.ref, l.err
}

func (f *fakeRemover) RemoveRunner(_ context.Context, id int64) error {
	f.removed = append(f.removed, id)
	return f.removeErr
}

// TestJITSourceRemoveRunnerMapsErrors pins the contract the scheduler relies
// on: only JobStillRunning reads as busy; a runner GitHub no longer knows is
// gone; an ambiguous DELETE failure is settled by asking GitHub again, and is
// ErrRemovalUnknown only when that also fails; any other error means the
// runner is still registered.
func TestJITSourceRemoveRunnerMapsErrors(t *testing.T) {
	ref := &scaleset.RunnerReference{ID: 42, Name: "r"}
	found := lookup{ref: ref}
	gone := lookup{}
	down := lookup{err: errors.New("503")}
	cases := []struct {
		name        string
		fake        fakeRemover
		wantErr     bool
		wantBusy    bool
		wantUnknown bool
		wantRemove  bool
	}{
		{"removed", fakeRemover{lookups: []lookup{found}}, false, false, false, true},
		{"unknown runner", fakeRemover{lookups: []lookup{gone}}, false, false, false, false},
		{"lookup failure", fakeRemover{lookups: []lookup{down}}, true, false, false, false},
		{"not found on remove", fakeRemover{lookups: []lookup{found}, removeErr: fmt.Errorf("req: %w: gone", scaleset.RunnerNotFoundError)}, false, false, false, true},
		{"job assigned", fakeRemover{lookups: []lookup{found}, removeErr: fmt.Errorf("req: %w: busy", scaleset.JobStillRunningError)}, true, true, false, true},
		{"remove failed, verify finds it", fakeRemover{lookups: []lookup{found, found}, removeErr: errors.New("503")}, true, false, false, true},
		{"remove failed, verify finds it gone", fakeRemover{lookups: []lookup{found, gone}, removeErr: context.DeadlineExceeded}, false, false, false, true},
		{"remove failed, verify fails", fakeRemover{lookups: []lookup{found, down}, removeErr: context.DeadlineExceeded}, true, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			j := &JITSource{remover: &f}
			err := j.RemoveRunner(context.Background(), "r")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got := errors.Is(err, core.ErrRunnerBusy); got != tc.wantBusy {
				t.Fatalf("errors.Is(err, ErrRunnerBusy)=%v want %v (err=%v)", got, tc.wantBusy, err)
			}
			if got := errors.Is(err, core.ErrRemovalUnknown); got != tc.wantUnknown {
				t.Fatalf("errors.Is(err, ErrRemovalUnknown)=%v want %v (err=%v)", got, tc.wantUnknown, err)
			}
			if got := len(f.removed) == 1 && f.removed[0] == 42; got != tc.wantRemove {
				t.Fatalf("removed=%v wantRemove=%v", f.removed, tc.wantRemove)
			}
		})
	}
}

func TestJobIdentityComesFromControlPlane(t *testing.T) {
	var got jobidentity.Job
	busy := false
	a := &scaler{log: testLogger(), onBusy: func(string) { busy = true }, onJobStarted: func(_ context.Context, j jobidentity.Job) {
		if !busy {
			t.Error("busy signal delayed behind identity")
		}
		got = j
	}}
	err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "runner", JobMessageBase: scaleset.JobMessageBase{OwnerName: "owner", RepositoryName: "repo", EventName: "pull_request", WorkflowRunID: 42, JobID: "job", JobWorkflowRef: "owner/repo/.github/workflows/ci.yml@refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}
	want := jobidentity.Job{WorkflowRef: "owner/repo/.github/workflows/ci.yml@refs/heads/main", Runner: "runner", Repository: "owner/repo", Event: "pull_request", RunID: 42, JobID: "job"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestJITSourceExists(t *testing.T) {
	for name, c := range map[string]struct {
		l          lookup
		want, fail bool
	}{
		"known":     {lookup{ref: &scaleset.RunnerReference{ID: 1}}, true, false},
		"forgotten": {lookup{}, false, false},
		"error":     {lookup{err: errors.New("503")}, false, true},
	} {
		j := &JITSource{remover: &fakeRemover{lookups: []lookup{c.l}}}
		got, err := j.Exists(context.Background(), "r")
		if got != c.want || (err != nil) != c.fail {
			t.Errorf("%s: Exists = %v, %v", name, got, err)
		}
	}
}

func TestJITSourceOwns(t *testing.T) {
	for _, c := range []struct {
		name string
		l    lookup
		want bool
		err  bool
	}{
		{"this scale set", lookup{ref: &scaleset.RunnerReference{ID: 1, RunnerScaleSetID: 7}}, true, false},
		{"another scale set", lookup{ref: &scaleset.RunnerReference{ID: 1, RunnerScaleSetID: 8}}, false, false},
		{"unknown", lookup{}, false, false},
		{"lookup failed", lookup{err: errors.New("503")}, false, true},
	} {
		j := &JITSource{remover: &fakeRemover{lookups: []lookup{c.l}}, scaleSetID: 7, namePrefix: "cloud"}
		got, err := j.Owns(context.Background(), "cloud-abc")
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: Owns = %v, %v", c.name, got, err)
		}
	}
	if p := (&JITSource{namePrefix: "cloud"}).NamePrefix(); p != "cloud-" {
		t.Errorf("NamePrefix = %q", p)
	}
}
