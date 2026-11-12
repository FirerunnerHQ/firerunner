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

// tenantRemoveTimeout bounds deleting a removed tenant's scale sets.
const tenantRemoveTimeout = 30 * time.Second

// tenantsReadTimeout bounds one read, so a hung store delays the next only.
const tenantsReadTimeout = 10 * time.Second

// errTenantsMissing is a source whose list does not exist (yet).
var errTenantsMissing = errors.New("the tenants list does not exist")

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

func (f fileTenants) Load(context.Context) ([]byte, error) {
	b, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", errTenantsMissing, err)
	}
	return b, err
}
func (f fileTenants) String() string { return string(f) }

// ssmGetAPI is the part of the SSM client a tenants list in a parameter uses.
type ssmGetAPI interface {
	GetParameter(context.Context, *awsssm.GetParameterInput, ...func(*awsssm.Options)) (*awsssm.GetParameterOutput, error)
}

// ssmTenants is a tenants list in one SSM parameter. A parameter that does
// not exist is errTenantsMissing.
type ssmTenants struct {
	name string
	api  ssmGetAPI
}

func (s ssmTenants) Load(ctx context.Context) ([]byte, error) {
	out, err := s.api.GetParameter(ctx, &awsssm.GetParameterInput{Name: aws.String(s.name), WithDecryption: aws.Bool(true)})
	var missing *ssmtypes.ParameterNotFound
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("parameter %s: %w", s.name, errTenantsMissing)
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
	// removed, when set, is called with a removed tenant's last entry once
	// its run has ended, whether that run served it or still waited on the
	// runs before it: it deletes what the tenant's tiers registered.
	removed func(config.Tenant)
	log     *slog.Logger

	mu      sync.Mutex
	running map[string]*tenantRun
	// ending are the runs stopped, removed or changed, that may still be
	// draining. A run starts only after every one of them on its id or its
	// account has ended: else one removed would delete the scale sets the
	// new one serves (scale sets are found by name, in the account), and
	// two would contend for one session.
	ending []*tenantRun
	// closed is set once wait is called: nothing starts after.
	closed bool
	// listed is set once a list has been applied.
	listed bool
	wg     sync.WaitGroup
}

// tenantRun is one tenant's loop in a tenantSet.
type tenantRun struct {
	// id and account are the tenant's, as listed when the run began.
	id, account string
	cur         atomic.Pointer[config.Tenant]
	cancel      context.CancelCauseFunc
	// done closes once the loop has ended, its runners finished.
	done chan struct{}
}

// apply makes the tenants served match list, which has been checked.
func (ts *tenantSet) apply(list []config.Tenant) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.closed || ts.ctx.Err() != nil {
		return
	}
	if ts.running == nil {
		ts.running = map[string]*tenantRun{}
	}
	ts.listed = true
	ts.ending = slices.DeleteFunc(ts.ending, func(r *tenantRun) bool {
		select {
		case <-r.done:
			return true
		default:
			return false
		}
	})
	want := map[string]bool{}
	for _, c := range list {
		want[c.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(ts.running)) {
		if !want[id] {
			ts.running[id].cancel(errTenantRemoved)
			ts.ending = append(ts.ending, ts.running[id])
			delete(ts.running, id)
			ts.log.Info("tenant removed from the list: it takes no more jobs, and its scale sets are deleted once its runners finish", "tenant", id)
		}
	}
	for _, c := range list {
		old, ok := ts.running[c.ID]
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
			ts.ending = append(ts.ending, old)
			ts.log.Info("tenant changed in the list: it starts again as listed once its runners finish", "tenant", c.ID)
		} else {
			ts.log.Info("tenant added to the list", "tenant", c.ID)
		}
		r := &tenantRun{id: c.ID, account: config.AccountOf(c.URL), done: make(chan struct{})}
		var prev []<-chan struct{}
		for _, e := range ts.ending {
			if e.id == r.id || e.account == r.account {
				prev = append(prev, e.done)
			}
		}
		r.cur.Store(&c)
		ctx, cancel := context.WithCancelCause(ts.ctx)
		r.cancel = cancel
		ts.running[c.ID] = r
		ts.wg.Go(func() {
			defer close(r.done)
			defer cancel(nil)
			// Before done: a run after it on the account must not register
			// what this deletes.
			defer func() {
				if removedTenant(ctx) && ts.removed != nil {
					ts.removed(*r.cur.Load())
				}
			}()
			// Whatever happens to this run, its done waits on the ones
			// before: a run after it must not start beside them.
			for _, p := range prev {
				<-p
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

// wait returns once every tenant's loop has ended. Nothing starts after it
// is called, whatever is applied.
func (ts *tenantSet) wait() {
	ts.mu.Lock()
	ts.closed = true
	ts.mu.Unlock()
	ts.wg.Wait()
}

// known reports whether id is a tenant listed now, or one stopped that may
// still have machines: GuardDuty findings on its machines flag it. Before
// any list is applied, every id is: a tenant's machines from before a
// restart may be running, and a finding passed over is not read again.
func (ts *tenantSet) known(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.listed {
		return true
	}
	if _, ok := ts.running[id]; ok {
		return true
	}
	return slices.ContainsFunc(ts.ending, func(r *tenantRun) bool { return r.id == id })
}

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
//
// A list that does not exist is no tenants until one has been applied, as
// on a new deployment. After that it is a failed read: a parameter deleted
// and written again, or a VM pointed at the wrong one, would otherwise stop
// every tenant and delete their scale sets. Removing them all takes an
// explicit [].
type tenantCatalog struct {
	src   tenantSource
	check func([]config.Tenant) error
	set   *tenantSet
	log   *slog.Logger
	// tick replaces the interval's ticker in tests.
	tick <-chan time.Time

	mu        sync.Mutex
	applied   bool
	lastRead  time.Time
	lastErr   string
	lastErrAt time.Time
}

// read reads, checks and applies the list once.
func (c *tenantCatalog) read(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, tenantsReadTimeout)
	defer cancel()
	b, err := c.src.Load(ctx)
	c.mu.Lock()
	if errors.Is(err, errTenantsMissing) && !c.applied {
		b, err = []byte("[]"), nil
	} else if errors.Is(err, errTenantsMissing) {
		err = fmt.Errorf("%w: the tenants served stay; write [] to remove them all", err)
	}
	c.mu.Unlock()
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
	c.mu.Lock()
	c.applied = true
	c.mu.Unlock()
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
