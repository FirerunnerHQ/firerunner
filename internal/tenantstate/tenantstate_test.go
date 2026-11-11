package tenantstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func TestCheckSpec(t *testing.T) {
	for _, ok := range []string{"/etc/firerunner/tenant-state.json", "ssm:/fr/control/tenant-state"} {
		if err := CheckSpec(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "tenant-state.json", "ssm:fr/x", "ssm:/fr/x/", "ssm:/fr//x", "ssm:"} {
		if err := CheckSpec(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{Install, "acme", "a-1"} {
		if !ValidID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "Acme", "_other", "-a", "a/b", strings.Repeat("a", 33)} {
		if ValidID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A file store starts empty, keeps what is put, forgets what is deleted, and
// a second store over the same file (a restarted daemon) reads the same.
func TestFileStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	f := &FileStore{Path: path}
	st, err := f.Load(ctx)
	if err != nil || len(st) != 0 {
		t.Fatalf("a missing file: %v, %v", st, err)
	}
	since := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := f.Put(ctx, "acme", Record{Reason: "abuse report", Since: since}); err != nil {
		t.Fatal(err)
	}
	if err := f.Put(ctx, Install, Record{Reason: "test", Since: since}); err != nil {
		t.Fatal(err)
	}
	again, err := (&FileStore{Path: path}).Load(ctx)
	if err != nil || len(again) != 2 || again["acme"].Reason != "abuse report" || !again["acme"].Since.Equal(since) {
		t.Fatalf("reloaded %v, %v", again, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, %v: the daemon may run as another user", fi.Mode(), err)
	}
	if err := f.Delete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "nobody"); err != nil {
		t.Fatalf("deleting what is not there: %v", err)
	}
	if st, _ := f.Load(ctx); len(st) != 1 || st.IDs()[0] != Install {
		t.Fatalf("after delete: %v", st)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(ctx); err == nil {
		t.Fatal("an empty file (a crash mid-write) read as no suspension")
	}
	if err := f.Put(ctx, "acme", Record{Reason: "again"}); err != nil {
		t.Fatalf("put over an empty file: %v", err)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(ctx); err == nil {
		t.Fatal("a file that does not parse: no error")
	}
	if err := f.Put(ctx, "acme", Record{}); err == nil {
		t.Fatal("put over a file that does not parse: it would drop the others")
	}
}

// fakeSSM keeps parameters in a map, pages by two, and can fail.
type fakeSSM struct {
	mu     sync.Mutex
	params map[string]types.Parameter
	err    error
}

func (f *fakeSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var names []string
	for n := range f.params {
		if strings.HasPrefix(n, aws.ToString(in.Path)+"/") && !strings.Contains(strings.TrimPrefix(n, aws.ToString(in.Path)+"/"), "/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	start := 0
	if in.NextToken != nil {
		start = int(aws.ToString(in.NextToken)[0] - '0')
	}
	end := min(start+2, len(names))
	out := &ssm.GetParametersByPathOutput{}
	for _, n := range names[start:end] {
		out.Parameters = append(out.Parameters, f.params[n])
	}
	if end < len(names) {
		out.NextToken = aws.String(string(rune('0' + end)))
	}
	return out, nil
}

func (f *fakeSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Type != types.ParameterTypeString || !aws.ToBool(in.Overwrite) {
		return nil, errors.New("want an overwritable String")
	}
	f.params[aws.ToString(in.Name)] = types.Parameter{Name: in.Name, Value: in.Value}
	return &ssm.PutParameterOutput{}, nil
}

func (f *fakeSSM) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.params[aws.ToString(in.Name)]; !ok {
		return nil, &types.ParameterNotFound{}
	}
	delete(f.params, aws.ToString(in.Name))
	return &ssm.DeleteParameterOutput{}, nil
}

// An SSM store keeps one parameter per suspended tenant under its path, reads
// every page, and leaves parameters outside its path alone.
func TestSSMStore(t *testing.T) {
	ctx := context.Background()
	api := &fakeSSM{params: map[string]types.Parameter{
		"/fr/control/tiers":        {Name: aws.String("/fr/control/tiers"), Value: aws.String("[]")},
		"/fr/control/tenant-state": {Name: aws.String("/fr/control/tenant-state"), Value: aws.String("x")},
	}}
	store, err := Open("ssm:/fr/control/tenant-state", func() (SSMAPI, error) { return api, nil })
	if err != nil {
		t.Fatal(err)
	}
	if store.String() != "ssm:/fr/control/tenant-state" {
		t.Fatalf("String = %q", store)
	}
	since := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b", "c", Install} {
		if err := store.Put(ctx, id, Record{Reason: "why " + id, Since: since}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Load(ctx)
	if err != nil || len(st) != 4 || st["c"].Reason != "why c" || !st[Install].Since.Equal(since) {
		t.Fatalf("loaded %v, %v", st, err)
	}
	if err := store.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "b"); err != nil {
		t.Fatalf("a second resume: %v", err)
	}
	if st, _ := store.Load(ctx); len(st) != 3 {
		t.Fatalf("after delete: %v", st)
	}
	if _, ok := api.params["/fr/control/tiers"]; !ok {
		t.Fatal("a parameter outside the path was touched")
	}

	// A record someone wrote by hand that does not parse still suspends.
	api.params["/fr/control/tenant-state/d"] = types.Parameter{Name: aws.String("/fr/control/tenant-state/d"), Value: aws.String("abuse")}
	st, _ = store.Load(ctx)
	if r, ok := st["d"]; !ok || !strings.Contains(r.Reason, "does not parse") {
		t.Fatalf("an unparsable record: %+v, %v", r, ok)
	}

	api.err = errors.New("AccessDeniedException")
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("a failing read: no error")
	}
}

// memStore is a Store whose states a test sets.
type memStore struct {
	mu  sync.Mutex
	st  States
	err error
}

func (m *memStore) Load(context.Context) (States, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	out := States{}
	for k, v := range m.st {
		out[k] = v
	}
	return out, nil
}
func (m *memStore) Put(context.Context, string, Record) error { return nil }
func (m *memStore) Delete(context.Context, string) error      { return nil }
func (m *memStore) String() string                            { return "mem" }

func (m *memStore) set(st States, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st, m.err = st, err
}

// The watcher reports the first read and every change, by the next tick
// after a write: the bound on how soon a running daemon learns of a
// suspension is one interval. A failed read keeps the last states.
func TestWatcherReportsChangesByTheNextTick(t *testing.T) {
	store := &memStore{st: States{}}
	changes := make(chan States, 8)
	tick := make(chan time.Time)
	w := &Watcher{Store: store, OnChange: func(st States) { changes <- st }, tick: tick}
	if _, known := w.States(); known {
		t.Fatal("known before any read")
	}
	if err := w.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-changes; len(got) != 0 {
		t.Fatalf("first read: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	store.set(States{"acme": {Reason: "abuse"}}, nil)
	tick <- time.Now()
	if got := <-changes; got["acme"].Reason != "abuse" {
		t.Fatalf("after the suspension's tick: %v", got)
	}

	tick <- time.Now() // nothing changed: nothing reported
	store.set(nil, errors.New("throttled"))
	tick <- time.Now()
	tick <- time.Now() // the failed read has been handled once this send is taken
	if st, known := w.States(); !known || st["acme"].Reason != "abuse" {
		t.Fatalf("a failed read changed the states: %v, %v", st, known)
	}
	if s := w.Status(); s.LastError != "throttled" || !s.Known {
		t.Fatalf("status = %+v", s)
	}
	select {
	case got := <-changes:
		t.Fatalf("reported %v without a change", got)
	default:
	}

	store.set(States{}, nil)
	tick <- time.Now()
	if got := <-changes; len(got) != 0 {
		t.Fatalf("after the resume's tick: %v", got)
	}
	cancel()
	<-done
}

func TestDefaultInterval(t *testing.T) {
	if DefaultInterval > 15*time.Second {
		t.Fatalf("DefaultInterval %s is longer than the 15 s the docs promise", DefaultInterval)
	}
}
