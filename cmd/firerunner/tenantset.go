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
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
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
	// errTenantHandedOff: this shard no longer holds the tenant's lease,
	// and another may serve it.
	errTenantHandedOff = errors.New("handed off: this shard's lease on it ended")
)

// removedTenant reports whether a tenant's loop ended because the list no
// longer names it, which deletes its scale sets. A change, or the daemon
// stopping, keeps them for the next start.
func removedTenant(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errTenantRemoved)
}

// handedOffTenant reports whether a tenant's loop ended because this shard
// no longer holds its lease. It closes its sessions at once, so the next
// shard can open them, and leaves its busy runners to that shard; its scale
// sets stay.
func handedOffTenant(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errTenantHandedOff)
}

// leaseKey carries a run's lapsed check in its context.
type leaseKey struct{}

// leaseLapsed reports whether the tenant's run serves past its lease's
// deadline: another shard may serve its account now, and register scale
// sets there under the names this run's would delete. False for a run with
// no lease.
func leaseLapsed(ctx context.Context) bool {
	lapsed, _ := ctx.Value(leaseKey{}).(func() bool)
	return lapsed != nil && lapsed()
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

// tenantsLoader is the job store's side of dbTenants; a *jobstore.TenantStore.
type tenantsLoader interface {
	Load(ctx context.Context) ([]byte, error)
}

// dbTenants is the tenants table of the job store's database. A database not
// set up yet is errTenantsMissing.
type dbTenants struct{ store tenantsLoader }

func (d dbTenants) Load(ctx context.Context) ([]byte, error) {
	b, err := d.store.Load(ctx)
	if errors.Is(err, jobstore.ErrNoTenants) {
		return nil, fmt.Errorf("%w: %w", errTenantsMissing, err)
	}
	return b, err
}

func (dbTenants) String() string { return config.TenantsDB + " (the job store's tenants table)" }

// openTenants returns the source --tenants names. newSSM is asked for a
// client only for a parameter, newDB for the job store only for db:.
func openTenants(spec string, newSSM func() (ssmGetAPI, error), newDB func() (tenantsLoader, error)) (tenantSource, error) {
	if spec == config.TenantsDB {
		store, err := newDB()
		if err != nil {
			return nil, err
		}
		return dbTenants{store: store}, nil
	}
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
//
// A shard that serves a tenant only while it holds the tenant's lease
// applies its list with applyLeases: a tenant is then served until a
// deadline, and handed off once that passes unrenewed, or once another
// shard holds its lease. A tenant changed there is handed off too, and
// starts again once its sessions close: its lease may go meanwhile.
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
	// runs before it: it deletes what the tenant's tiers registered, calls
	// namesGone, then ends what it left on EC2. ctx ends with the set's;
	// its leaseLapse, with the run's lease. An error means it did not all
	// go.
	removed func(ctx context.Context, c config.Tenant, namesGone func()) error
	// forget, when set, is called once removed has deleted all a tenant
	// left, with the time its removal was seen and its URL: the job store
	// forgets its removal on that URL recorded by then, so that no shard
	// sweeps it again. An error leaves the removal to sweep again.
	forget func(id string, upTo time.Time, url string) error
	log    *slog.Logger
	// now and afterFunc replace the clock and its timers in tests.
	now       func() time.Time
	afterFunc func(time.Duration, func()) (stop func() bool)

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
	// listed is set once a list has been applied, and ids holds the ids
	// it lists: on a leased shard, some of them served by other shards.
	listed bool
	ids    map[string]bool
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
	// deadline, when set, is the time the run may launch and serve until,
	// on the monotonic clock: its lease's. stopTimer stops the timer that
	// hands it off then; it is guarded by the set's mu.
	deadline  atomic.Pointer[time.Time]
	stopTimer func() bool
	// removing is set once the run ends because its tenant was removed:
	// it deletes the tenant's scale sets once its runners finish. Guarded
	// by the set's mu. removedAt is when its removal was seen, or, for a
	// sweep (tenantSet.sweep), when it was recorded.
	removing  bool
	removedAt time.Time
	// swept is set on a run that only deletes what a tenant removed while
	// no shard served it left.
	swept bool
	// named, on a removing run, closes once its scale sets are deleted
	// (or their deletion gave up): a run of another id on the account
	// waits for that alone, not for its EC2 sweep, which goes by this id's
	// owner tag. Set before the run is cancelled; guarded by the set's mu.
	named chan struct{}
}

// waitsOn is what a run of id on account must wait for, of run e: all of
// it on the same id, its scale sets' deletion on the same account.
func (e *tenantRun) waitsOn(id, account string) <-chan struct{} {
	switch {
	case e.id == id:
		return e.done
	case e.account != account:
		return nil
	case e.named != nil:
		return e.named
	}
	return e.done
}

// fence returns why the run may launch nothing more, or "" while it may:
// for the scheduler (scheduler.Tenant.SetFence).
func (r *tenantRun) fence(now func() time.Time) func() string {
	return func() string {
		if d := r.deadline.Load(); d != nil && !now().Before(*d) {
			return fmt.Sprintf("this shard's lease on tenant %q was not renewed by %s", r.id, d.UTC().Format(time.TimeOnly))
		}
		return ""
	}
}

// expired reports whether the run's deadline has passed.
func (r *tenantRun) expired(now time.Time) bool {
	d := r.deadline.Load()
	return d != nil && !now.Before(*d)
}

// clock is the time now.
func (ts *tenantSet) clock() time.Time {
	if ts.now != nil {
		return ts.now()
	}
	return time.Now()
}

// endLocked stops a run, for cause, and moves it from running to those
// ending. A run handed off is fenced at once: a launch on its way must not
// get past the fence while its deadline is still ahead. The caller holds
// ts.mu.
func (ts *tenantSet) endLocked(r *tenantRun, cause error) {
	if errors.Is(cause, errTenantHandedOff) {
		now := ts.clock()
		r.deadline.Store(&now)
	}
	r.removing = errors.Is(cause, errTenantRemoved)
	if r.removing {
		r.removedAt = time.Now()
		r.named = make(chan struct{})
	}
	r.cancel(cause)
	if r.stopTimer != nil {
		r.stopTimer()
	}
	ts.ending = append(ts.ending, r)
	delete(ts.running, r.id)
}

// renewLocked sets the run's deadline, zero for none, and arms the timer
// that hands it off once the deadline passes unrenewed. The caller holds
// ts.mu.
func (ts *tenantSet) renewLocked(r *tenantRun, deadline time.Time) {
	if r.stopTimer != nil {
		r.stopTimer()
		r.stopTimer = nil
	}
	if deadline.IsZero() {
		r.deadline.Store(nil)
		return
	}
	r.deadline.Store(&deadline)
	after := ts.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	r.stopTimer = after(deadline.Sub(ts.clock()), func() { ts.expire(r) })
}

// expire hands a run off if its deadline has passed unrenewed: from then on,
// another shard may hold its lease. The timer acts even when whatever
// renews leases is stuck, as on a database that does not answer.
func (ts *tenantSet) expire(r *tenantRun) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.running[r.id] != r {
		return // ended already
	}
	if !r.expired(ts.clock()) {
		// Renewed since, or a clock that moved: wait for the deadline again.
		ts.renewLocked(r, *r.deadline.Load())
		return
	}
	ts.log.Warn("tenant's lease not renewed in time: handing it off; launches stop, and its busy runners finish", "tenant", r.id)
	ts.endLocked(r, errTenantHandedOff)
}

// apply makes the tenants served match list, which has been checked.
func (ts *tenantSet) apply(list []config.Tenant) { ts.applyLeases(list, nil) }

// applyLeases is apply on a shard that serves a tenant only under its lease.
// list is every tenant, and leases the deadline of each this shard holds the
// lease of, on the monotonic clock. A tenant of the list is served until its
// deadline, which a later call renews; one this shard serves without a
// lease, or past its deadline, is handed off. A tenant missing from the list
// is removed, leased or not. A nil leases serves every tenant listed, with no
// deadline.
//
// A run handed off is not revived by a lease renewed after it: another shard
// may have served the tenant meanwhile. A new run starts instead, once the
// old one has closed its sessions.
func (ts *tenantSet) applyLeases(list []config.Tenant, leases map[string]time.Time) {
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
	ts.ids = want
	for _, id := range slices.Sorted(maps.Keys(ts.running)) {
		if !want[id] {
			ts.endLocked(ts.running[id], errTenantRemoved)
			ts.log.Info("tenant removed from the list: it takes no more jobs, and its scale sets are deleted once its runners finish", "tenant", id)
		}
	}
	now := ts.clock()
	for _, c := range list {
		deadline, leased := leases[c.ID]
		if leases == nil {
			leased = true
		} else if leased && !now.Before(deadline) {
			leased = false
		}
		old, ok := ts.running[c.ID]
		if ok && (!leased || old.expired(now)) {
			ts.endLocked(old, errTenantHandedOff)
			ok = false
			ts.log.Info("tenant handed off: this shard no longer holds its lease; its sessions close, and its busy runners finish", "tenant", c.ID)
		}
		if !leased {
			continue
		}
		if ok {
			was := *old.cur.Load()
			switch {
			case reflect.DeepEqual(was, c):
				ts.renewLocked(old, deadline)
				continue
			case onlyRunnersDiffer(was, c):
				old.cur.Store(&c)
				ts.renewLocked(old, deadline)
				ts.setMax(c.ID, c.EffectiveLimits().Runners)
				ts.log.Info("tenant's runner limit changed", "tenant", c.ID,
					"from", was.EffectiveLimits().Runners, "to", c.EffectiveLimits().Runners)
				continue
			}
			if leases != nil {
				// Ended as a hand-off: its lease may go meanwhile, and the
				// next run, here or on another shard, must not wait on its
				// sessions until its runners finish.
				ts.endLocked(old, errTenantHandedOff)
				ts.log.Info("tenant changed in the list: it starts again as listed once its sessions close", "tenant", c.ID)
			} else {
				ts.endLocked(old, errTenantChanged)
				ts.log.Info("tenant changed in the list: it starts again as listed once its runners finish", "tenant", c.ID)
			}
		} else if leases == nil {
			ts.log.Info("tenant added to the list", "tenant", c.ID)
		} else {
			ts.log.Info("tenant's lease taken: serving it", "tenant", c.ID)
		}
		r := &tenantRun{id: c.ID, account: config.AccountOf(c.URL), done: make(chan struct{})}
		var prev []<-chan struct{}
		for _, e := range ts.ending {
			if ch := e.waitsOn(r.id, r.account); ch != nil {
				prev = append(prev, ch)
			}
		}
		r.cur.Store(&c)
		ctx, cancel := context.WithCancelCause(context.WithValue(context.WithValue(ts.ctx,
			leaseKey{}, func() bool { return r.expired(ts.clock()) }),
			boundKey{}, func() (context.Context, func()) { return ts.leaseBound(r) }))
		r.cancel = cancel
		ts.running[c.ID] = r
		ts.renewLocked(r, deadline)
		fence := r.fence(ts.clock)
		ts.wg.Go(func() {
			defer close(r.done)
			defer cancel(nil)
			// Before done: a run after it on the account must not register
			// what this deletes.
			defer func() {
				if removedTenant(ctx) {
					_ = ts.remove(r)
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
				// Before any tier serves: nothing launches past the lease.
				st.bound.SetFence(fence)
			})
		})
	}
}

// runningAccounts returns the accounts of the runs serving now.
func (ts *tenantSet) runningAccounts() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var accounts []string
	for _, r := range ts.running {
		accounts = append(accounts, r.account)
	}
	return accounts
}

// renewRemoving moves the deadline of each run removed that has not ended
// to its account's in held, the leases just renewed: it deletes its scale
// sets only while its lease holds. One whose account is not in held keeps
// its deadline, which lapses; one whose deadline has passed is not
// revived, as another shard may have served the account meanwhile.
func (ts *tenantSet) renewRemoving(held map[string]time.Time) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	now := ts.clock()
	for _, r := range ts.ending {
		if d, ok := held[r.account]; ok && r.removing && r.deadline.Load() != nil && !r.expired(now) {
			r.deadline.Store(&d)
		}
	}
}

// errLeaseLapsed ends a removal whose lease has lapsed, and
// errListedAgain one of a tenant id listed again.
var (
	errLeaseLapsed = errors.New("the lease on the account lapsed")
	errListedAgain = errors.New("the tenant is listed again")
)

// lapseKey carries a removal's lapse context in its context.
type lapseKey struct{}

// leaseLapse returns the context that ends once a removal's lease lapses,
// or nil for one under no lease. It is apart from the removal's own
// context, which ends as the daemon stops: a lapse must stop a removal
// then too, when nothing renews its lease any more.
func leaseLapse(ctx context.Context) context.Context {
	l, _ := ctx.Value(lapseKey{}).(context.Context)
	return l
}

// remove deletes what removed run r's tenant left, while r's lease holds,
// and forgets its removal once all of it is gone. A removal whose lease
// lapses first, or that fails, deletes no more and is not forgotten: with
// the tenants in the job store, a shard sweeps it again later. It returns
// why it did not all go.
func (ts *tenantSet) remove(r *tenantRun) error {
	if ts.removed == nil {
		return nil
	}
	if r.expired(ts.clock()) {
		ts.log.Warn("removed tenant's lease lapsed before what it left was deleted: leaving it, as another shard may serve its account; it is swept again under a lease", "tenant", r.id)
		return errLeaseLapsed
	}
	ctx, stop := ts.leaseBound(r)
	defer stop()
	c := *r.cur.Load()
	namesGone := sync.OnceFunc(func() {
		if r.named != nil {
			close(r.named)
		}
	})
	defer namesGone()
	if err := ts.removed(ctx, c, namesGone); err != nil {
		ts.log.Warn("removed tenant's leftovers not all deleted: kept to sweep again", "tenant", r.id, "err", err)
		return err
	}
	if ts.forget != nil {
		if err := ts.forget(r.id, r.removedAt, c.URL); err != nil {
			ts.log.Warn("removed tenant's leftovers deleted, but its removal not forgotten: swept again later", "tenant", r.id, "err", err)
			return err
		}
	}
	return nil
}

// boundKey carries a run's leaseBound in its context: a removal of what
// it serves, at its stop, is bound by its lease too.
type boundKey struct{}

// boundRemoval returns a context of ctx's for a removal of what a run
// served, bounded by tenantRemoveTimeout, and ended at once if the run's
// lease lapses, the daemon stopping or not; and its release.
func boundRemoval(ctx context.Context) (context.Context, func()) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tenantRemoveTimeout)
	bound, ok := ctx.Value(boundKey{}).(func() (context.Context, func()))
	if !ok {
		return rctx, cancel
	}
	lctx, stop := bound()
	unhook := func() bool { return false }
	if lapse := leaseLapse(lctx); lapse != nil {
		unhook = context.AfterFunc(lapse, cancel)
	}
	return rctx, func() { unhook(); stop(); cancel() }
}

// leaseBound returns a context of the set's whose leaseLapse ends, with
// cause errLeaseLapsed, once r's deadline passes unrenewed, whether or not
// the set's has ended; and the func that releases it. A run with no
// deadline has none.
func (ts *tenantSet) leaseBound(r *tenantRun) (context.Context, func()) {
	if r.deadline.Load() == nil {
		return ts.ctx, func() {}
	}
	lapse, cancel := context.WithCancelCause(context.WithoutCancel(ts.ctx))
	ctx := context.WithValue(ts.ctx, lapseKey{}, context.Context(lapse))
	after := ts.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	var mu sync.Mutex
	var stopTimer func() bool
	var arm func()
	arm = func() {
		d := r.deadline.Load()
		if d == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if lapse.Err() != nil {
			return
		}
		stopTimer = after(d.Sub(ts.clock()), func() {
			if r.expired(ts.clock()) {
				cancel(errLeaseLapsed)
				return
			}
			arm() // renewed since
		})
	}
	arm()
	return ctx, func() {
		cancel(nil)
		mu.Lock()
		defer mu.Unlock()
		if stopTimer != nil {
			stopTimer()
		}
	}
}

// sweep starts a run that only deletes what tenant c, removed and not yet
// cleared, left: once every run before it on its id or account has ended,
// and while this shard's lease on its account holds, which runs to
// deadline and is renewed as a removing run's. A run of the account listed
// after it waits for it. One serving the account here now goes on beside
// it, and its tiers' scale sets, which it took over by name, are not
// deleted. removedAt is when the removal was recorded, and done, when set,
// is called with how the sweep ended. It reports false, and starts
// nothing, while this id's removal is under way here, or the set has
// stopped.
func (ts *tenantSet) sweep(c config.Tenant, removedAt, deadline time.Time, done func(error)) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.closed || ts.ctx.Err() != nil {
		return false
	}
	account := config.AccountOf(c.URL)
	for _, r := range ts.ending {
		select {
		case <-r.done:
			continue
		default:
		}
		if r.id == c.ID && r.removing {
			return false
		}
	}
	if r, ok := ts.running[c.ID]; ok && r.account == account {
		return false // listed again: its removal is no more
	}
	for _, r := range ts.running {
		// Scale sets are found by name in a runner group: only the same
		// group's names are that run's.
		if cur := r.cur.Load(); r.account == account && cur.RunnerGroup == c.RunnerGroup {
			c.Tiers = slices.DeleteFunc(slices.Clone(c.Tiers), func(t string) bool { return slices.Contains(cur.Tiers, t) })
		}
	}
	r := &tenantRun{id: c.ID, account: account, done: make(chan struct{}), removing: true, removedAt: removedAt, swept: true, named: make(chan struct{})}
	r.cur.Store(&c)
	r.deadline.Store(&deadline)
	r.cancel = func(error) {}
	var prev []<-chan struct{}
	for _, e := range ts.ending {
		if ch := e.waitsOn(r.id, r.account); ch != nil {
			prev = append(prev, ch)
		}
	}
	ts.ending = append(ts.ending, r)
	ts.log.Info("tenant removed while no shard served it: deleting what it left", "tenant", c.ID, "account", account)
	ts.wg.Go(func() {
		defer close(r.done)
		for _, p := range prev {
			<-p
		}
		err := ts.remove(r)
		if done != nil {
			done(err)
		}
	})
	return true
}

// sweeping returns the ids of the sweeps not yet ended.
func (ts *tenantSet) sweeping() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var ids []string
	for _, r := range ts.ending {
		select {
		case <-r.done:
		default:
			if r.swept {
				ids = append(ids, r.id)
			}
		}
	}
	return ids
}

// removing returns the accounts of the runs removed that have not ended
// yet: each deletes its scale sets, by name in its account, once its
// runners finish, so the account must not be served elsewhere until then.
func (ts *tenantSet) removing() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var accounts []string
	for _, r := range ts.ending {
		select {
		case <-r.done:
		default:
			if r.removing {
				accounts = append(accounts, r.account)
			}
		}
	}
	return accounts
}

// wait returns once every tenant's loop has ended. Nothing starts after it
// is called, whatever is applied.
func (ts *tenantSet) wait() {
	ts.mu.Lock()
	ts.closed = true
	for _, r := range ts.running {
		if r.stopTimer != nil {
			r.stopTimer()
		}
	}
	ts.mu.Unlock()
	ts.wg.Wait()
}

// known reports whether id is a tenant listed now, served here or by
// another shard, or one stopped that may still have machines: GuardDuty
// findings on its machines flag it. Before any list is applied, every id
// is: a tenant's machines from before a restart may be running, and a
// finding passed over is not read again.
func (ts *tenantSet) known(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.listed || ts.ids[id] {
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
	// leaser, when set, applies each list instead of set: under leases.
	leaser *tenantLeaser
	log    *slog.Logger
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
		if c.leaser != nil {
			c.leaser.listFailed()
		}
		return err
	}
	if c.leaser != nil {
		c.leaser.apply(list)
	} else {
		c.set.apply(list)
	}
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
