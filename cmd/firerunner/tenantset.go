package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
)

// tenantsInterval is how often the daemon reads its tenants list: a tenant
// added there is served within this long.
const tenantsInterval = 30 * time.Second

// tenantsReadTimeout bounds one read, so a hung store delays the next only.
const tenantsReadTimeout = 10 * time.Second

// Why a tenant's loop ends while the daemon runs: the cause of its context.
var (
	errTenantRemoved = errors.New("removed from the tenants list")
	errTenantChanged = errors.New("changed in the tenants list")
)

// removedTenant reports whether a tenant's loop ended because the list no
// longer names it, which deletes its scale sets. A change, or the daemon
// stopping, keeps them for the next start.
func removedTenant(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errTenantRemoved)
}

// tenantSource is where the tenants list is kept.
type tenantSource interface {
	// Load returns the list as written.
	Load(ctx context.Context) ([]byte, error)
	String() string
}

// fileTenants is a tenants list in a file.
type fileTenants string

func (f fileTenants) Load(context.Context) ([]byte, error) { return os.ReadFile(string(f)) }
func (f fileTenants) String() string                       { return string(f) }

// ssmGetAPI is the part of the SSM client a tenants list in a parameter uses.
type ssmGetAPI interface {
	GetParameter(context.Context, *awsssm.GetParameterInput, ...func(*awsssm.Options)) (*awsssm.GetParameterOutput, error)
}

// ssmTenants is a tenants list in one SSM parameter. A parameter that does
// not exist lists no tenants: deleting it is how an operator removes the
// last one. Any other failure is a failed read.
type ssmTenants struct {
	name string
	api  ssmGetAPI
}

func (s ssmTenants) Load(ctx context.Context) ([]byte, error) {
	out, err := s.api.GetParameter(ctx, &awsssm.GetParameterInput{Name: aws.String(s.name), WithDecryption: aws.Bool(true)})
	var missing *ssmtypes.ParameterNotFound
	if errors.As(err, &missing) {
		return []byte("[]"), nil
	}
	if err != nil {
		return nil, err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return nil, fmt.Errorf("parameter %s has no value", s.name)
	}
	return []byte(*out.Parameter.Value), nil
}

func (s ssmTenants) String() string { return config.TenantsSSM + s.name }

// openTenants returns the source --tenants names. newSSM is asked for a
// client only for a parameter.
func openTenants(spec string, newSSM func() (ssmGetAPI, error)) (tenantSource, error) {
	name, ok := config.TenantsParameter(spec)
	if !ok {
		return fileTenants(spec), nil
	}
	api, err := newSSM()
	if err != nil {
		return nil, err
	}
	return ssmTenants{name: name, api: api}, nil
}

// tenantSet serves the tenants of the last list read, each in a loop of its
// own, and makes them match each list applied:
//
//   - a tenant added starts;
//   - a tenant removed stops taking jobs, its runners finish the ones they
//     have, and its scale sets are deleted;
//   - a tenant whose runner limit alone changed gets the new limit in place;
//   - a tenant changed otherwise stops as a removed one does, but keeps its
//     scale sets, and starts again as listed once its runners have finished.
//     Its jobs queue meanwhile: one scale set takes one session at a time.
type tenantSet struct {
	ctx context.Context
	// run serves a tenant until ctx ends. cur returns its entry as listed
	// now, and served is to be called with each start once it serves.
	run func(ctx context.Context, cur func() config.Tenant, served func(*servedTenant))
	// setMax sets the runner limit of the tenant served by that id, if one
	// is.
	setMax func(id string, max int)
	log    *slog.Logger

	mu      sync.Mutex
	running map[string]*tenantRun
	// removed are the runs of tenants removed, which may still drain: a
	// tenant listed again starts after its last run has ended, or that
	// run would delete the scale sets the new one serves.
	removed map[string]*tenantRun
	wg      sync.WaitGroup
}

// tenantRun is one tenant's loop in a tenantSet.
type tenantRun struct {
	cur    atomic.Pointer[config.Tenant]
	cancel context.CancelCauseFunc
	// done closes once the loop has ended, its runners finished.
	done chan struct{}
}

// apply makes the tenants served match list, which has been checked.
func (ts *tenantSet) apply(list []config.Tenant) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.running == nil {
		ts.running, ts.removed = map[string]*tenantRun{}, map[string]*tenantRun{}
	}
	want := map[string]bool{}
	for _, c := range list {
		want[c.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(ts.running)) {
		if !want[id] {
			ts.running[id].cancel(errTenantRemoved)
			ts.removed[id] = ts.running[id]
			delete(ts.running, id)
			ts.log.Info("tenant removed from the list: it takes no more jobs, and its scale sets are deleted once its runners finish", "tenant", id)
		}
	}
	for _, c := range list {
		old, ok := ts.running[c.ID]
		var prev <-chan struct{}
		if ok {
			was := *old.cur.Load()
			switch {
			case reflect.DeepEqual(was, c):
				continue
			case onlyRunnersDiffer(was, c):
				old.cur.Store(&c)
				ts.setMax(c.ID, c.EffectiveLimits().Runners)
				ts.log.Info("tenant's runner limit changed", "tenant", c.ID,
					"from", was.EffectiveLimits().Runners, "to", c.EffectiveLimits().Runners)
				continue
			}
			old.cancel(errTenantChanged)
			prev = old.done
			ts.log.Info("tenant changed in the list: it starts again as listed once its runners finish", "tenant", c.ID)
		} else {
			if gone, ok := ts.removed[c.ID]; ok {
				prev = gone.done
				delete(ts.removed, c.ID)
			}
			ts.log.Info("tenant added to the list", "tenant", c.ID)
		}
		r := &tenantRun{done: make(chan struct{})}
		r.cur.Store(&c)
		ctx, cancel := context.WithCancelCause(ts.ctx)
		r.cancel = cancel
		ts.running[c.ID] = r
		ts.wg.Go(func() {
			defer close(r.done)
			defer cancel(nil)
			if prev != nil {
				// Whatever happens to this run, its done waits on the one
				// before: a run after it must not start beside that one.
				<-prev
			}
			if ctx.Err() != nil {
				return
			}
			cur := func() config.Tenant { return *r.cur.Load() }
			ts.run(ctx, cur, func(st *servedTenant) {
				// A limit changed while it started reaches it here: apply
				// stores the entry before it looks for the tenant served.
				st.bound.SetMax(cur().EffectiveLimits().Runners)
			})
		})
	}
}

// wait returns once every tenant's loop has ended.
func (ts *tenantSet) wait() { ts.wg.Wait() }

// onlyRunnersDiffer reports whether two entries of one tenant differ in
// their runner limit alone, which changes in place.
func onlyRunnersDiffer(a, b config.Tenant) bool {
	la, lb := a.EffectiveLimits(), b.EffectiveLimits()
	if la.VCPU != lb.VCPU || la.JobDuration != lb.JobDuration {
		return false
	}
	a.Limits, b.Limits = nil, nil
	return reflect.DeepEqual(a, b)
}

// tenantCatalog reads the tenants list now and every tenantsInterval, and
// applies each list that reads and checks. A list that does not leaves the
// tenants as they were, and says why in health.
type tenantCatalog struct {
	src   tenantSource
	check func([]config.Tenant) error
	set   *tenantSet
	log   *slog.Logger
	// tick replaces the interval's ticker in tests.
	tick <-chan time.Time

	mu        sync.Mutex
	lastRead  time.Time
	lastErr   string
	lastErrAt time.Time
}

// read reads, checks and applies the list once.
func (c *tenantCatalog) read(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, tenantsReadTimeout)
	defer cancel()
	b, err := c.src.Load(ctx)
	var list []config.Tenant
	if err == nil {
		if list, err = config.ParseTenants(b); err == nil {
			err = c.check(list)
		}
	}
	now := time.Now()
	c.mu.Lock()
	if err != nil {
		c.lastErr, c.lastErrAt = err.Error(), now
	} else {
		c.lastRead = now
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("tenants list not applied; serving the tenants as they were", "source", c.src.String(), "err", err)
		return err
	}
	c.set.apply(list)
	return nil
}

// run reads the list every tenantsInterval until ctx ends. It does not read
// at once: call read first.
func (c *tenantCatalog) run(ctx context.Context) {
	tick := c.tick
	if tick == nil {
		t := time.NewTicker(tenantsInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			_ = c.read(ctx)
		}
	}
}

// status is how reading the list goes, for health.
func (c *tenantCatalog) status() *health.TenantStates {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &health.TenantStates{Store: c.src.String(), LastRead: c.lastRead, LastError: c.lastErr, LastErrorAt: c.lastErrAt}
}
