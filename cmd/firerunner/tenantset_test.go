package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
)

// setLab runs a tenantSet whose tenants record their starts and ends, and
// end only once the test lets them: a tenant's runners finishing their jobs.
type setLab struct {
	t   *testing.T
	set *tenantSet

	mu     sync.Mutex
	starts chan tenantStart
	ends   chan tenantEnd
	gates  map[string]chan struct{}
	// over is set once the test ends: every gate is open from then on.
	over  bool
	maxes []string
}

type tenantStart struct {
	id  string
	cur func() config.Tenant
}

type tenantEnd struct {
	id    string
	cause error
}

func newSetLab(t *testing.T) *setLab {
	ctx, cancel := context.WithCancel(context.Background())
	l := &setLab{t: t, starts: make(chan tenantStart, 16), ends: make(chan tenantEnd, 16), gates: map[string]chan struct{}{}}
	l.set = &tenantSet{
		ctx: ctx, log: slog.New(slog.DiscardHandler),
		run: func(ctx context.Context, cur func() config.Tenant, _ func(*servedTenant)) {
			id := cur().ID
			l.starts <- tenantStart{id, cur}
			<-ctx.Done()
			l.ends <- tenantEnd{id, context.Cause(ctx)}
			g := l.gate(id)
			<-g
			// The gate is this run's: the next run of the tenant, which
			// starts only after this one returns, gets a new one.
			l.mu.Lock()
			if l.gates[id] == g {
				delete(l.gates, id)
			}
			l.mu.Unlock()
		},
		setMax: func(id string, max int) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.maxes = append(l.maxes, id+"="+strconv.Itoa(max))
		},
	}
	t.Cleanup(func() {
		cancel()
		l.mu.Lock()
		l.over = true
		for _, g := range l.gates {
			select {
			case <-g:
			default:
				close(g)
			}
		}
		l.mu.Unlock()
		l.set.wait()
	})
	return l
}

// gate is what a tenant's run waits on once its context ends.
func (l *setLab) gate(id string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, ok := l.gates[id]
	if !ok {
		g = make(chan struct{})
		if l.over {
			close(g)
			return g
		}
		l.gates[id] = g
	}
	return g
}

// finish lets a tenant's ended run return.
func (l *setLab) finish(id string) {
	close(l.gate(id)) // the run may not have asked for it yet
}

func (l *setLab) started(want string) tenantStart {
	l.t.Helper()
	select {
	case s := <-l.starts:
		if s.id != want {
			l.t.Fatalf("started %s, want %s", s.id, want)
		}
		return s
	case <-time.After(5 * time.Second):
		l.t.Fatalf("%s never started", want)
	}
	return tenantStart{}
}

// startedAll waits for tenants that start at once, in any order.
func (l *setLab) startedAll(want ...string) {
	l.t.Helper()
	got := map[string]bool{}
	for range want {
		select {
		case s := <-l.starts:
			got[s.id] = true
		case <-time.After(5 * time.Second):
			l.t.Fatalf("started %v, want %v", got, want)
		}
	}
	for _, id := range want {
		if !got[id] {
			l.t.Fatalf("started %v, want %v", got, want)
		}
	}
}

func (l *setLab) ended(want string, cause error) {
	l.t.Helper()
	select {
	case e := <-l.ends:
		if e.id != want || !errors.Is(e.cause, cause) {
			l.t.Fatalf("ended %s (%v), want %s (%v)", e.id, e.cause, want, cause)
		}
	case <-time.After(5 * time.Second):
		l.t.Fatalf("%s never ended", want)
	}
}

// nothing checks that no tenant starts or ends: a start would have been sent
// before apply returned, as the runs need nothing else to start.
func (l *setLab) nothing() {
	l.t.Helper()
	select {
	case s := <-l.starts:
		l.t.Fatalf("%s started", s.id)
	case e := <-l.ends:
		l.t.Fatalf("%s ended (%v)", e.id, e.cause)
	case <-time.After(50 * time.Millisecond):
	}
}

func listed(id string, runners int, tiers ...string) config.Tenant {
	return config.Tenant{ID: id, URL: "https://github.com/" + id, InstallationID: 7, Tiers: tiers,
		Limits: &config.TenantLimits{MaxRunners: &runners}}
}

// Tenants start, stop and change to match the list applied. One removed
// ends as removed (its scale sets go); one changed ends as changed (they
// stay), and starts again as listed only once its runners have finished; one
// whose runner limit alone changed keeps serving, with the new limit.
func TestTenantSetFollowsTheList(t *testing.T) {
	l := newSetLab(t)
	l.set.apply([]config.Tenant{listed("a", 2, "x"), listed("b", 2, "x")})
	l.startedAll("a", "b")

	l.set.apply([]config.Tenant{listed("a", 2, "x"), listed("b", 2, "x")})
	l.nothing()

	l.set.apply([]config.Tenant{listed("a", 2, "x")})
	l.ended("b", errTenantRemoved)
	l.finish("b")

	l.set.apply([]config.Tenant{listed("a", 5, "x")})
	l.nothing()
	if got := strings.Join(l.maxes, ","); got != "a=5" {
		t.Fatalf("set max %s, want a=5", got)
	}

	l.set.apply([]config.Tenant{listed("a", 5, "x", "y")})
	l.ended("a", errTenantChanged)
	l.nothing() // its runners have not finished
	l.finish("a")
	s := l.started("a")
	if got := s.cur().Tiers; len(got) != 2 {
		t.Fatalf("restarted with tiers %v, want the new list's", got)
	}
}

// A tenant removed, then listed again while its runners finish, starts only
// once they have: else its old run would delete the scale sets the new one
// serves.
func TestTenantSetWaitsForARemovedTenant(t *testing.T) {
	l := newSetLab(t)
	l.set.apply([]config.Tenant{listed("a", 2, "x")})
	l.started("a")
	l.set.apply(nil)
	l.ended("a", errTenantRemoved)
	l.set.apply([]config.Tenant{listed("a", 2, "x")})
	l.nothing()
	l.finish("a")
	l.started("a")
}

// A change that comes while the last is still waiting for runners to finish
// replaces it: only the latest entry starts, once.
func TestTenantSetChangesInTurn(t *testing.T) {
	l := newSetLab(t)
	l.set.apply([]config.Tenant{listed("a", 2, "x")})
	l.started("a")
	l.set.apply([]config.Tenant{listed("a", 2, "y")})
	l.ended("a", errTenantChanged)
	l.set.apply([]config.Tenant{listed("a", 2, "z")})
	l.nothing()
	l.finish("a")
	s := l.started("a")
	if got := s.cur().Tiers[0]; got != "z" {
		t.Fatalf("started with tier %s, want the latest, z", got)
	}
	l.nothing()
}

func TestOnlyRunnersDiffer(t *testing.T) {
	two, five := 2, 5
	a := config.Tenant{ID: "a", Tiers: []string{"x"}}
	b := a
	b.Limits = &config.TenantLimits{MaxRunners: &five}
	if !onlyRunnersDiffer(a, b) {
		t.Fatal("a runner limit set: not a change in place")
	}
	b.Limits = &config.TenantLimits{MaxRunners: &two}
	if !onlyRunnersDiffer(a, b) {
		t.Fatal("the default written out: not a change in place")
	}
	vcpu := 4
	b.Limits = &config.TenantLimits{MaxVCPU: &vcpu}
	if onlyRunnersDiffer(a, b) {
		t.Fatal("a vCPU limit changes which tiers it gets: it needs a restart")
	}
	d := config.Duration(time.Hour)
	b.Limits = &config.TenantLimits{MaxJobDuration: &d}
	if onlyRunnersDiffer(a, b) {
		t.Fatal("a job limit is the schedulers': it needs a restart")
	}
	b = a
	b.InstallationID = 8
	if onlyRunnersDiffer(a, b) {
		t.Fatal("another installation: it needs a restart")
	}
}

// fakeSource is a tenants list in memory.
type fakeSource struct {
	b   []byte
	err error
}

func (f *fakeSource) Load(context.Context) ([]byte, error) { return f.b, f.err }
func (f *fakeSource) String() string                       { return "fake" }

// A list that does not read, parse or check leaves the tenants served as
// they were, and says why in health; the next good one applies.
func TestTenantCatalogKeepsTheTenantsOnABadList(t *testing.T) {
	l := newSetLab(t)
	src := &fakeSource{b: []byte(`[{"id":"a","url":"https://github.com/a","installation_id":7,"tiers":["x"]}]`)}
	checked := 0
	c := &tenantCatalog{src: src, set: l.set, log: slog.New(slog.DiscardHandler),
		check: func(ts []config.Tenant) error {
			checked++
			if len(ts) > 0 && ts[0].ID == "rejected" {
				return errors.New("tenant \"rejected\": no")
			}
			return nil
		}}
	if err := c.read(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.started("a")
	if st := c.status(); st.Failing() || st.LastRead.IsZero() || st.Store != "fake" {
		t.Fatalf("status %+v", st)
	}

	for name, s := range map[string]fakeSource{
		"unreadable":    {err: errors.New("AccessDenied")},
		"not JSON":      {b: []byte(`[{"id":`)},
		"unknown field": {b: []byte(`[{"id":"a","colour":"red"}]`)},
		"rejected":      {b: []byte(`[{"id":"rejected"}]`)},
	} {
		*src = s
		if err := c.read(context.Background()); err == nil {
			t.Fatalf("%s: read", name)
		}
		if st := c.status(); !st.Failing() {
			t.Fatalf("%s: status %+v, want failing", name, st)
		}
	}
	l.nothing()

	*src = fakeSource{b: []byte(`[]`)}
	if err := c.read(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.ended("a", errTenantRemoved)
	l.finish("a")
	if st := c.status(); st.Failing() {
		t.Fatalf("status %+v after a good read", st)
	}
}

// fakeGet is an SSM GetParameter.
type fakeGet struct {
	value *string
	err   error
}

func (f fakeGet) GetParameter(_ context.Context, in *awsssm.GetParameterInput, _ ...func(*awsssm.Options)) (*awsssm.GetParameterOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &awsssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Name: in.Name, Value: f.value}}, nil
}

// A parameter that does not exist lists no tenants; one that cannot be read
// is a failed read, which keeps them.
func TestSSMTenants(t *testing.T) {
	v := `[{"id":"a"}]`
	src, err := openTenants("ssm:/fr/control/tenants", func() (ssmGetAPI, error) { return fakeGet{value: &v}, nil })
	if err != nil || src.String() != "ssm:/fr/control/tenants" {
		t.Fatalf("open: %v, %v", src, err)
	}
	if b, err := src.Load(context.Background()); err != nil || string(b) != v {
		t.Fatalf("load: %q, %v", b, err)
	}
	gone := ssmTenants{name: "/x", api: fakeGet{err: &ssmtypes.ParameterNotFound{}}}
	if b, err := gone.Load(context.Background()); err != nil || string(b) != "[]" {
		t.Fatalf("missing parameter: %q, %v; want no tenants", b, err)
	}
	denied := ssmTenants{name: "/x", api: fakeGet{err: errors.New("AccessDeniedException")}}
	if _, err := denied.Load(context.Background()); err == nil {
		t.Fatal("a denied read listed no tenants: it would remove them all")
	}
	if src, _ := openTenants("/etc/firerunner/tenants.json", nil); src.String() != "/etc/firerunner/tenants.json" {
		t.Fatalf("file source %v", src)
	}
}

// Only a tenant removed from the list loses its scale sets.
func TestRemovedTenant(t *testing.T) {
	for cause, want := range map[error]bool{errTenantRemoved: true, errTenantChanged: false, context.Canceled: false} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		if got := removedTenant(ctx); got != want {
			t.Errorf("%v: removed %v, want %v", cause, got, want)
		}
	}
	// The daemon stopping ends the set's context, not the tenant's own.
	parent, stop := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stop()
	if removedTenant(ctx) {
		t.Error("a daemon stopping removed a tenant")
	}
}

// Health reports the runner limit in force, which the list may have changed
// since the tenant started.
func TestServedTenantHealthHasTheLimitInForce(t *testing.T) {
	st := &servedTenant{t: tenant{ID: "a", Limits: config.Limits{Runners: 2}}, bound: scheduler.NewTenant("a", 2)}
	st.bound.SetMax(7)
	if got := st.health(nil).Limits.Runners; got != 7 {
		t.Fatalf("runners %d, want 7", got)
	}
}

// What health says of a tenant not served goes once its loop ends.
func TestLiveTenantsForget(t *testing.T) {
	live := &liveTenants{}
	live.skip(tenant{ID: "a"}, "no tier")
	live.skip(tenant{ID: "b"}, "no tier")
	live.wait("a", errors.New("down"), time.Second)
	live.forget("a")
	got := live.tenants(nil, nil)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("tenants %+v, want b alone", got)
	}
	if n := len(live.trackers()); n != 0 {
		t.Fatalf("%d trackers left", n)
	}
}
