package listener

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	sslistener "github.com/actions/scaleset/listener"

	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/health"
	"github.com/solcreek/firerunner/internal/jobidentity"
)

// Config configures the scale-set listener.
type Config struct {
	URL         string
	Name        string
	RunnerGroup string
	Labels      []string
	MaxRunners  int
	MinRunners  int

	// Auth: a personal access token, or a GitHub App (preferred, per GitHub).
	Token string
	App   scaleset.GitHubAppAuth

	// SystemInfo metadata reported to GitHub.
	Version   string
	CommitSHA string

	OnJobStarted func(context.Context, jobidentity.Job)
	Logger       *slog.Logger
	// Health, if set, records the tier's connection state.
	Health *health.Tracker
}

// session is one open message session; *scaleset.MessageSessionClient
// satisfies it.
type session interface {
	sslistener.Client
	Close(ctx context.Context) error
}

// ScaleSet is the production Listener. Constructing it registers an ephemeral
// runner scale set with GitHub; Close deregisters it.
type ScaleSet struct {
	cfg    Config
	log    *slog.Logger
	client *scaleset.Client
	// removeClient serves only runner deregistration. scaleset.Client holds one
	// non-context-aware mutex across every call, so sharing client would let a
	// slow JIT generation stall a scale-down past its deadline, and a slow
	// removal delay the next scale-up. The scheduler issues one removal at a
	// time per tier, so nothing ever waits on this client's mutex.
	removeClient *scaleset.Client
	// jitClients generates JIT configs in parallel; see clientPool.
	jitClients *clientPool
	// stats holds the latest statistics GitHub sent with a message.
	stats statsRecorder
	// mu guards recycle, the cancel func of the current session's context.
	mu      sync.Mutex
	recycle context.CancelFunc
	session session
	// open opens a fresh message session after the current one fails.
	open       func(context.Context) (session, error)
	health     *health.Tracker
	scaleSetID int
	// created is true only when this process created the scale set (rather than
	// reusing a pre-existing one). Close deletes the scale set only when created,
	// so it never deregisters a set another instance is relying on.
	created bool
}

// New builds a scaleset client, registers the runner scale set, and opens a
// message session. Call Close to deregister.
func New(ctx context.Context, cfg Config, owner string) (*ScaleSet, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.RunnerGroup == "" {
		cfg.RunnerGroup = scaleset.DefaultRunnerGroup
	}

	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	removeClient, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	// Message sessions get a client of their own too: every session request
	// (open, refresh, close) takes its parent client's mutex, which a JIT
	// generation holds across its network round trip. During a GitHub outage
	// that would queue a reconnect's close and reopen behind stuck JIT
	// requests, past their own deadlines.
	sessionClient, err := newClient(cfg)
	if err != nil {
		return nil, err
	}

	groupID, err := runnerGroupID(ctx, client, cfg.RunnerGroup)
	if err != nil {
		return nil, err
	}

	id, reused, err := registerScaleSet(ctx, client, cfg, groupID, removeClient, sessionClient)
	if err != nil {
		return nil, err
	}

	first, err := openSessionWithRetry(ctx, sessionClient, id, owner, cfg.Logger)
	if err != nil {
		if !reused {
			// Best-effort cleanup of the scale set we just created.
			_ = client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), id)
		}
		return nil, fmt.Errorf("open message session: %w", err)
	}

	cfg.Logger.Info("registered runner scale set",
		"name", cfg.Name, "scaleSetID", id, "group", cfg.RunnerGroup, "reused", reused)

	s := &ScaleSet{
		cfg: cfg, log: cfg.Logger, client: client, removeClient: removeClient,
		session: first, health: cfg.Health,
		scaleSetID: id, created: !reused,
	}
	// The registration client is warm (it holds a token): it seeds the pool.
	s.jitClients = newClientPool(jitParallelism(cfg.MaxRunners), func() (jitClient, error) {
		c, err := newClient(cfg)
		if err != nil {
			return nil, err
		}
		c.SetSystemInfo(systemInfo(cfg, id))
		return c, nil
	})
	s.jitClients.seed(client)
	s.open = func(ctx context.Context) (session, error) {
		sess, err := openSessionWithRetry(ctx, sessionClient, id, owner, cfg.Logger)
		if err != nil {
			return nil, err
		}
		return sess, nil
	}
	return s, nil
}

// registerScaleSet gets or creates the tier's scale set through client and
// returns its ID and whether it already existed. Reusing an existing set (from
// a prior, possibly unclean, run) keeps restarts idempotent instead of failing
// with "runner scale set already exists". Every client, including others,
// reports the set's ID in its system info.
func registerScaleSet(ctx context.Context, client *scaleset.Client, cfg Config, groupID int, others ...*scaleset.Client) (int, bool, error) {
	desired := &scaleset.RunnerScaleSet{
		Name:          cfg.Name,
		RunnerGroupID: groupID,
		Labels:        buildLabels(cfg.Name, cfg.Labels),
		// Ephemeral golden images are rebuilt out of band; the runner agent must
		// not self-update (GitHub official guidance for image-based runners).
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
	existing, err := client.GetRunnerScaleSet(ctx, groupID, cfg.Name)
	if err != nil {
		return 0, false, fmt.Errorf("look up runner scale set: %w", err)
	}
	var scaleSet *scaleset.RunnerScaleSet
	reused := existing != nil
	if reused {
		scaleSet, err = client.UpdateRunnerScaleSet(ctx, existing.ID, desired)
		if err != nil {
			return 0, false, fmt.Errorf("update runner scale set: %w", err)
		}
	} else {
		scaleSet, err = client.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return 0, false, fmt.Errorf("create runner scale set: %w", err)
		}
	}
	for _, c := range append([]*scaleset.Client{client}, others...) {
		c.SetSystemInfo(systemInfo(cfg, scaleSet.ID))
	}
	return scaleSet.ID, reused, nil
}

// openSessionWithRetry opens a message session, retrying while GitHub reports a
// session conflict. After an unclean shutdown (SIGKILL, OOM, power loss) the
// previous session stays active server-side until it times out (~1 min), so a
// restarting service would otherwise crash-loop. Retrying lets it self-heal.
// Any non-conflict error (e.g. bad auth) fails fast.
func openSessionWithRetry(ctx context.Context, client *scaleset.Client, scaleSetID int, owner string, log *slog.Logger) (*scaleset.MessageSessionClient, error) {
	const attempts = 12
	const delay = 10 * time.Second

	var err error
	for i := 0; i < attempts; i++ {
		var session *scaleset.MessageSessionClient
		session, err = client.MessageSessionClient(ctx, scaleSetID, owner)
		if err == nil {
			return session, nil
		}
		if !isSessionConflict(err) {
			return nil, err
		}
		log.Warn("scale set session still held by a previous run; waiting for it to expire",
			"attempt", i+1, "maxAttempts", attempts)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, err
}

// isSessionConflict reports whether err is GitHub's "scale set already has an
// active session" (HTTP 409) response. The library surfaces it only as a
// wrapped string, so we match on the conflict marker.
func isSessionConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "conflict")
}

// JIT returns a JIT source bound to this scale set, satisfying
// scheduler.JITSource.
func (s *ScaleSet) JIT() *JITSource {
	return &JITSource{clients: s.jitClients, remover: s.removeClient, scaleSetID: s.scaleSetID, namePrefix: s.cfg.Name}
}

// ErrFatal marks a listener error that reconnecting cannot fix: GitHub
// rejected the credentials. It stops that tier alone.
var ErrFatal = errors.New("listener stopped")

// ErrRestart marks a listener error that only a fresh process recovers from:
// the scale set is gone. That happens when an instance that created it
// deleted it on shutdown while this one reused it, as in an overlapping
// redeploy. Startup registers the set again, so the daemon exits for its
// supervisor to restart it.
var ErrRestart = errors.New("runner scale set is gone; restart to register it again")

// Reconnect pacing: the wait between attempts to re-open a session doubles
// from reconnectMin to reconnectMax, with jitter. A session that stayed up for
// reconnectReset counts as healthy, so the next failure starts from the
// minimum again. Vars so tests can shorten them.
var (
	reconnectMin   = 2 * time.Second
	reconnectMax   = 2 * time.Minute
	reconnectReset = time.Minute
)

// Run implements Listener: long-poll GitHub and drive onDesired until ctx is
// cancelled.
//
// When the session fails (GitHub unreachable past the library's own retries,
// an expired session), Run closes it and opens a new one with backoff, without
// returning: the tier's scheduler and its runners are untouched, and the new
// session's statistics re-drive the desired count. Run ends only on an error
// retrying cannot fix: rejected credentials (ErrFatal, which stops this tier
// alone) or a deleted scale set (ErrRestart, which restarts the daemon).
func (s *ScaleSet) Run(ctx context.Context, onDesired DesiredFunc, onBusy BusyFunc, capacity CapacityFunc) error {
	sess := s.session
	backoff := reconnectMin
	for {
		started := time.Now()
		err := s.runSession(ctx, sess, onDesired, onBusy, capacity)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) >= reconnectReset {
			backoff = reconnectMin
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = sess.Close(closeCtx)
		cancel()
		for {
			wait := jitter(backoff)
			s.log.Warn("listener session ended; reconnecting", "err", err, "backoff", wait)
			if s.health != nil {
				s.health.Reconnecting(err, wait)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			backoff = min(2*backoff, reconnectMax)
			sess, err = s.open(ctx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isFatal(err) {
				s.log.Error("listener cannot reconnect; this tier stops taking jobs", "err", err)
				if s.health != nil {
					s.health.Failed(err)
				}
				return fmt.Errorf("%w: %w", ErrFatal, err)
			}
			if isScaleSetGone(err) {
				s.log.Error("runner scale set is gone; restarting to register it again", "err", err)
				if s.health != nil {
					s.health.Failed(err)
				}
				return fmt.Errorf("%w: %w", ErrRestart, err)
			}
		}
		s.session = sess
		s.log.Info("listener reconnected")
		if s.health != nil {
			s.health.Reconnected()
		}
	}
}

// runSession listens on one session until it fails or ctx ends.
func (s *ScaleSet) runSession(ctx context.Context, sess session, onDesired DesiredFunc, onBusy BusyFunc, capacity CapacityFunc) error {
	// Recycle ends this session (and only this one) to make Run reconnect.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.recycle = cancel
	s.mu.Unlock()
	l, err := sslistener.New(trackedSession{session: sess, health: s.health, chaos: chaosExitAfterDelete()}, sslistener.Config{
		ScaleSetID: s.scaleSetID,
		MaxRunners: s.cfg.MaxRunners,
		Logger:     s.log.WithGroup("scaleset"),
	}, sslistener.WithMetricsRecorder(&s.stats))
	if err != nil {
		return fmt.Errorf("create scaleset listener: %w", err)
	}
	a := &scaler{
		onDesired:    onDesired,
		onJobStarted: s.cfg.OnJobStarted,
		onBusy:       onBusy,
		minRunners:   s.cfg.MinRunners,
		maxRunners:   s.cfg.MaxRunners,
		log:          s.log,
	}
	if capacity != nil {
		a.capacity = capacity
		a.setCapacity = l.SetMaxRunners
		a.advertiseCapacity()
	}
	return l.Run(ctx, a)
}

// Stats returns the scale set statistics GitHub sent with the latest
// message, and when they arrived; ok is false before the first.
func (s *ScaleSet) Stats() (stats scaleset.RunnerScaleSetStatistic, at time.Time, ok bool) {
	return s.stats.latest()
}

// Recycle ends the current message session, so Run closes it and opens a new
// one as after any session failure. A fresh session gets GitHub's current
// statistics, and possibly re-offers jobs the old one never acquired.
func (s *ScaleSet) Recycle(reason string) {
	s.mu.Lock()
	cancel := s.recycle
	s.mu.Unlock()
	if cancel != nil {
		s.log.Warn("recycling the message session", "reason", reason)
		cancel()
	}
}

// statsRecorder keeps the latest statistics GitHub sent (an
// sslistener.MetricsRecorder).
type statsRecorder struct {
	mu    sync.Mutex
	stats scaleset.RunnerScaleSetStatistic
	at    time.Time
	ok    bool
}

func (r *statsRecorder) RecordStatistics(st *scaleset.RunnerScaleSetStatistic) {
	if st == nil {
		return
	}
	r.mu.Lock()
	r.stats, r.at, r.ok = *st, time.Now(), true
	r.mu.Unlock()
}
func (r *statsRecorder) RecordJobStarted(*scaleset.JobStarted)     {}
func (r *statsRecorder) RecordJobCompleted(*scaleset.JobCompleted) {}
func (r *statsRecorder) RecordDesiredRunners(int)                  {}

func (r *statsRecorder) latest() (scaleset.RunnerScaleSetStatistic, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats, r.at, r.ok
}

// chaosExitAfterDelete reports whether FR_CHAOS_EXIT_AFTER_DELETE is set: a
// fault-injection switch for testing recovery, never for production. The
// process then exits right after GitHub accepts a message's deletion, before
// the message is acted on. actions/scaleset deletes a message first, then
// acquires its available jobs and applies its statistics, so this is the
// moment a crash loses the message.
func chaosExitAfterDelete() bool { return os.Getenv("FR_CHAOS_EXIT_AFTER_DELETE") == "1" }

// trackedSession records each successful GetMessage in the tier's health.
type trackedSession struct {
	session
	health *health.Tracker
	// chaos exits the process after DeleteMessage (see chaosExitAfterDelete).
	chaos bool
}

func (t trackedSession) DeleteMessage(ctx context.Context, messageID int) error {
	err := t.session.DeleteMessage(ctx, messageID)
	if err == nil && t.chaos {
		fmt.Fprintf(os.Stderr, "firerunner: FR_CHAOS_EXIT_AFTER_DELETE: exiting after deleting message %d, before acting on it\n", messageID)
		os.Exit(137)
	}
	return err
}

func (t trackedSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	m, err := t.session.GetMessage(ctx, lastMessageID, maxCapacity)
	if err == nil && t.health != nil {
		t.health.MessageOK()
	}
	return m, err
}

// isFatal reports whether a session error is one retrying cannot fix: GitHub
// rejected the credentials (401/403). The library reports status only in the
// error text. A 403 for rate limiting is transient, not fatal.
func isFatal(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "rate limit") {
		return false
	}
	for _, status := range []string{"401 unauthorized", "403 forbidden"} {
		if strings.Contains(msg, status) {
			return true
		}
	}
	return false
}

// isScaleSetGone reports whether opening a session failed because GitHub no
// longer knows the scale set (404).
func isScaleSetGone(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "404 not found")
}

// jitter spreads d by ±20% so tiers that failed together do not reconnect in
// lockstep.
func jitter(d time.Duration) time.Duration {
	var b [2]byte
	_, _ = rand.Read(b[:])
	f := 0.8 + 0.4*float64(int(b[0])<<8|int(b[1]))/65535
	return time.Duration(float64(d) * f)
}

// Close deregisters the scale set and closes the message session. It deletes the
// scale set only when this process created it; a scale set we merely reused (it
// existed from a prior run or belongs to another instance) is left in place so
// closing one instance never tears out a set another is depending on.
func (s *ScaleSet) Close(ctx context.Context) error {
	_ = s.session.Close(ctx)
	if !s.created {
		s.log.Info("leaving reused runner scale set registered", "scaleSetID", s.scaleSetID)
		return nil
	}
	if err := s.client.DeleteRunnerScaleSet(ctx, s.scaleSetID); err != nil {
		return fmt.Errorf("delete runner scale set: %w", err)
	}
	s.log.Info("deregistered runner scale set", "scaleSetID", s.scaleSetID)
	return nil
}

// scaler adapts GitHub's scale-set callbacks onto firerunner's DesiredFunc.
// Because every microVM is ephemeral and self-terminating, job start/completion
// mark the VM busy for shutdown drain and dispatch trusted identity resolution.
// JobStarted is a second, independent busy signal alongside the guest console
// marker the provisioner watches: the console marker fires the instant the guest
// dequeues a job (the primary, earliest source), while the scale-set protocol's
// JobStarted can arrive later, but OR-ing them is strictly safe — a busy flag
// only ever makes the drain wait for a real job, never cancels one early.
//
// capacity/setCapacity close the loop in the other direction: the library sends
// GitHub an X-ScaleSetMaxCapacity header on every poll, and GitHub assigns no
// more jobs than that to the scale set. The header defaults to the tier's static
// max, which is only honest when the tier has the host to itself; advertising
// the live capacity instead lets a host at the shared slot cap decline work
// that a sibling scale set (same name, another runner group) could start now.
type scaler struct {
	onJobStarted func(context.Context, jobidentity.Job)
	onDesired    DesiredFunc
	onBusy       BusyFunc
	capacity     CapacityFunc
	setCapacity  func(int)
	minRunners   int
	maxRunners   int
	log          *slog.Logger
	lastAdvert   int
}

var _ sslistener.Scaler = (*scaler)(nil)

func (a *scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	desired := a.minRunners + count
	running := a.onDesired(ctx, desired)
	a.advertiseCapacity()
	return running, nil
}

// advertiseCapacity pushes the current CapacityFunc value to the library for
// its next GetMessage. The value is clamped to [1, maxRunners]: the ceiling is
// the tier's own max, and the floor avoids sending 0, whose meaning to the
// Actions back-end is undocumented (ARC never sends it).
func (a *scaler) advertiseCapacity() {
	if a.capacity == nil || a.setCapacity == nil {
		return
	}
	c := a.capacity()
	if c > a.maxRunners {
		c = a.maxRunners
	}
	if c < 1 {
		c = 1
	}
	if c != a.lastAdvert {
		a.log.Info("advertising capacity", "capacity", c, "max", a.maxRunners)
		a.lastAdvert = c
	}
	a.setCapacity(c)
}

func (a *scaler) HandleJobStarted(ctx context.Context, j *scaleset.JobStarted) error {
	a.log.Info("job started", "runner", j.RunnerName, "runnerRequestId", j.RunnerRequestID)
	if a.onBusy != nil && j.RunnerName != "" {
		a.onBusy(j.RunnerName)
	}
	if a.onJobStarted != nil {
		a.onJobStarted(ctx, jobidentity.Job{WorkflowRef: j.JobWorkflowRef, Runner: j.RunnerName, Repository: j.OwnerName + "/" + j.RepositoryName, Event: j.EventName, RunID: j.WorkflowRunID, JobID: j.JobID})
	}
	return nil
}

func (a *scaler) HandleJobCompleted(_ context.Context, j *scaleset.JobCompleted) error {
	a.log.Info("job completed", "runner", j.RunnerName, "result", j.Result)
	return nil
}

// JITSource generates just-in-time runner registrations against the scale set.
type JITSource struct {
	clients    *clientPool
	remover    runnerRemover
	scaleSetID int
	namePrefix string
}

// Generate implements scheduler.JITSource.
func (j *JITSource) Generate(ctx context.Context, _ core.RunnerSpec) (name, jitConfig string, err error) {
	c, err := j.clients.get(ctx)
	if err != nil {
		return "", "", fmt.Errorf("generate JIT config: %w", err)
	}
	defer j.clients.put(c)
	name = j.namePrefix + "-" + randSuffix()
	cfg, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, j.scaleSetID)
	if err != nil {
		return "", "", fmt.Errorf("generate JIT config: %w", err)
	}
	return name, cfg.EncodedJITConfig, nil
}

// jitClient is the part of *scaleset.Client that Generate uses.
type jitClient interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// maxJITClients caps one tier's concurrent JIT requests to GitHub.
const maxJITClients = 32

// jitParallelism sizes a tier's JIT client pool: one per runner the tier can
// launch at once, up to maxJITClients.
func jitParallelism(maxRunners int) int {
	return min(max(maxRunners, 1), maxJITClients)
}

// clientPool hands out JIT clients so concurrent launches do not queue on one.
// A scaleset.Client holds a mutex across every request, so a burst of launches
// sharing one client generates its configs one after another: 20 queued jobs
// waited up to ~20 round trips for their runners. The pool grows on demand up
// to its limit, reusing idle clients (and their cached tokens) first, and
// beyond that waits for one to free up.
type clientPool struct {
	idle    chan jitClient
	mu      sync.Mutex
	created int
	limit   int
	make    func() (jitClient, error)
}

func newClientPool(limit int, mk func() (jitClient, error)) *clientPool {
	return &clientPool{idle: make(chan jitClient, limit), limit: limit, make: mk}
}

// get returns an idle client, a new one while under the limit, or waits.
func (p *clientPool) get(ctx context.Context) (jitClient, error) {
	select {
	case c := <-p.idle:
		return c, nil
	default:
	}
	p.mu.Lock()
	if p.created < p.limit {
		p.created++
		p.mu.Unlock()
		c, err := p.make()
		if err != nil {
			p.mu.Lock()
			p.created--
			p.mu.Unlock()
			return nil, err
		}
		return c, nil
	}
	p.mu.Unlock()
	select {
	case c := <-p.idle:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// seed adds an existing client (the warm registration client), counted
// toward the limit like one the pool made.
func (p *clientPool) seed(c jitClient) {
	p.mu.Lock()
	p.created++
	p.mu.Unlock()
	p.put(c)
}

// put returns a client the pool handed out.
func (p *clientPool) put(c jitClient) { p.idle <- c }

// Exists reports whether GitHub still knows the named runner.
func (j *JITSource) Exists(ctx context.Context, name string) (bool, error) {
	opCtx, cancel := context.WithTimeout(ctx, removeOpTimeout)
	defer cancel()
	r, err := j.remover.GetRunnerByName(opCtx, name)
	if err != nil {
		return false, err
	}
	return r != nil, nil
}

// runnerRemover is the part of *scaleset.Client that RemoveRunner uses, so the
// error mapping can be tested without GitHub.
type runnerRemover interface {
	GetRunnerByName(ctx context.Context, runnerName string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// removeOpTimeout bounds each request of a removal. The scheduler calls
// RemoveRunner only when that removal can run, so none of it is spent queued.
// A var so tests can shorten it.
var removeOpTimeout = 15 * time.Second

// RemoveRunner implements scheduler.RunnerRemover: it deregisters the named
// runner, returning core.ErrRunnerBusy when GitHub refuses because the runner
// already has a job assigned. A runner GitHub no longer knows is already gone,
// so that is success.
//
// Any other DELETE failure is ambiguous: a timeout or lost response can follow
// a removal GitHub already applied. So it asks GitHub again. Gone is success,
// still there is a plain error (the VM may take jobs), and no answer at all is
// core.ErrRemovalUnknown, which the scheduler retries.
func (j *JITSource) RemoveRunner(ctx context.Context, name string) error {
	opCtx, cancel := context.WithTimeout(ctx, removeOpTimeout)
	defer cancel()
	r, err := j.remover.GetRunnerByName(opCtx, name)
	if err != nil {
		// Nothing was sent that could remove it, so it is still registered.
		return fmt.Errorf("look up runner %q: %w", name, err)
	}
	if r == nil {
		return nil
	}
	err = j.remover.RemoveRunner(opCtx, int64(r.ID))
	switch {
	case err == nil, errors.Is(err, scaleset.RunnerNotFoundError):
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		return fmt.Errorf("remove runner %q: %w: %w", name, core.ErrRunnerBusy, err)
	}

	// opCtx may be what failed; verify on a fresh budget.
	vCtx, vCancel := context.WithTimeout(ctx, removeOpTimeout)
	defer vCancel()
	still, verr := j.remover.GetRunnerByName(vCtx, name)
	switch {
	case verr != nil:
		return fmt.Errorf("remove runner %q: %w: %w (verify: %w)", name, core.ErrRemovalUnknown, err, verr)
	case still == nil:
		return nil
	default:
		return fmt.Errorf("remove runner %q: %w", name, err)
	}
}

// --- helpers ---

func newClient(cfg Config) (*scaleset.Client, error) {
	if cfg.App.Validate() == nil {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.URL,
			GitHubAppAuth:   cfg.App,
			SystemInfo:      systemInfo(cfg, 0),
		})
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     cfg.URL,
		PersonalAccessToken: cfg.Token,
		SystemInfo:          systemInfo(cfg, 0),
	})
}

func runnerGroupID(ctx context.Context, client *scaleset.Client, group string) (int, error) {
	if group == scaleset.DefaultRunnerGroup {
		return 1, nil
	}
	rg, err := client.GetRunnerGroupByName(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("get runner group %q: %w", group, err)
	}
	return rg.ID, nil
}

// buildLabels always advertises the scale set name as a label so a job can
// reach the tier with runs-on: <name>; extra labels are additive (GitHub routes
// jobs by matching runs-on against the advertised labels, so dropping the name
// would make the tier unreachable by its own name).
func buildLabels(name string, labels []string) []scaleset.Label {
	out := []scaleset.Label{{Name: name}}
	for _, l := range labels {
		if l != name {
			out = append(out, scaleset.Label{Name: l})
		}
	}
	return out
}

func systemInfo(cfg Config, scaleSetID int) scaleset.SystemInfo {
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	commit := cfg.CommitSHA
	if commit == "" {
		commit = "NA"
	}
	return scaleset.SystemInfo{
		System:     "firerunner",
		Subsystem:  "listener",
		Version:    version,
		CommitSHA:  commit,
		ScaleSetID: scaleSetID,
	}
}

func randSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
