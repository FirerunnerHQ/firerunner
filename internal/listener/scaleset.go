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

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
)

// Config configures the scale-set listener.
type Config struct {
	URL         string
	Name        string
	RunnerGroup string
	Labels      []string
	MaxRunners  int
	// KeepOnClose leaves a scale set this process created registered when
	// it closes. A long-lived install (a stable --owner, a control VM that
	// is replaced) keeps one scale set across restarts: deleting it on
	// every graceful stop would make each deploy recreate it.
	KeepOnClose bool

	// Auth: a personal access token, or a GitHub App (preferred, per GitHub).
	Token string
	App   scaleset.GitHubAppAuth

	// SystemInfo metadata reported to GitHub.
	Version   string
	CommitSHA string

	OnJobStarted func(context.Context, jobidentity.Job)
	// OnJobOffered, if set, is told each job GitHub offers or assigns to the
	// scale set (JobAvailable, JobAssigned), as it arrives: the cache's
	// identity discovery takes its candidate runs from them.
	OnJobOffered func(jobidentity.Job)
	// Events, if set, is told of each job's assignment, start and end.
	Events jobevent.Sink
	// Pending, if set, keeps the tier's assigned-but-not-started jobs, read
	// from each message before the library acts on it.
	Pending *assigned.Jobs
	Logger  *slog.Logger
	// Health, if set, records the tier's connection state.
	Health *health.Tracker
}

// session is one open message session; *scaleset.MessageSessionClient
// satisfies it.
type session interface {
	sslistener.Client
	Close(ctx context.Context) error
}

// ScaleSet is the production Listener. Constructing it registers a runner
// scale set with GitHub, or reuses one of the same name; Close deregisters
// one it created, unless Config.KeepOnClose.
type ScaleSet struct {
	cfg    Config
	log    *slog.Logger
	client *scaleset.Client
	// removeClient serves only runner deregistration. actions/scaleset v0.4.0
	// held one non-context-aware mutex across every call, and v0.5.0 still holds
	// one across a token refresh, so sharing client could let a slow refresh
	// stall a scale-down past its deadline. The scheduler issues one removal at a
	// time per tier; the reconciler's lost-runner lookups and offline-runner
	// removals (rare: one per leaked runner) are its only other callers.
	removeClient *scaleset.Client
	// jitClients generates JIT configs in parallel; see clientPool.
	jitClients *clientPool
	// stats holds the latest statistics GitHub sent with a message.
	stats statsRecorder
	// mu guards recycle, the cancel func of the live session's context; nil
	// when no session is live.
	mu      sync.Mutex
	recycle context.CancelCauseFunc
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
	// Message sessions get a client of their own too. In v0.4.0 every session
	// request took its parent client's mutex, which a JIT generation held across
	// its round trip; v0.5.0 holds one only across a token refresh. Either way a
	// stuck call on a shared client would hold up a reconnect.
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
func (s *ScaleSet) Run(ctx context.Context, onDesired DesiredFunc, onBusy BusyFunc, onCompleted CompletedFunc, capacity CapacityFunc) error {
	sess := s.session
	backoff := reconnectMin
	for {
		started := time.Now()
		err := s.runSession(ctx, sess, onDesired, onBusy, onCompleted, capacity)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) >= reconnectReset {
			backoff = reconnectMin
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = sess.Close(closeCtx)
		cancel()
		if errors.Is(err, errRecycled) {
			// Asked for, not a failure: reopen at once, with no backoff and
			// nothing recorded against the listener's health. Only if the
			// reopen fails does the usual reconnect path take over.
			if sess, err = s.open(ctx); err == nil {
				s.session = sess
				s.log.Info("listener session recycled")
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
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
func (s *ScaleSet) runSession(ctx context.Context, sess session, onDesired DesiredFunc, onBusy BusyFunc, onCompleted CompletedFunc, capacity CapacityFunc) error {
	// Recycle ends this session (and only this one) to make Run reopen it.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s.mu.Lock()
	s.recycle = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.recycle = nil
		s.mu.Unlock()
	}()
	ts := trackedSession{session: sess, health: s.health, pending: s.cfg.Pending, events: s.cfg.Events, tier: s.cfg.Name, offered: s.cfg.OnJobOffered, log: s.log, chaos: chaosExitAfterDelete()}
	l, err := sslistener.New(ts, sslistener.Config{
		ScaleSetID: s.scaleSetID,
		MaxRunners: s.cfg.MaxRunners,
		Logger:     s.log.WithGroup("scaleset"),
	})
	if err != nil {
		return fmt.Errorf("create scaleset listener: %w", err)
	}
	a := &scaler{
		onDesired:    onDesired,
		onJobStarted: s.cfg.OnJobStarted,
		onBusy:       onBusy,
		onCompleted:  onCompleted,
		events:       s.cfg.Events,
		tier:         s.cfg.Name,
		maxRunners:   s.cfg.MaxRunners,
		log:          s.log,
		stats:        &s.stats,
		acquire:      ts.AcquireJobs,
	}
	if capacity != nil {
		a.capacity = capacity
		a.setCapacity = l.SetMaxRunners
		a.advertiseCapacity()
	}
	err = l.Run(ctx, a)
	if errors.Is(context.Cause(ctx), errRecycled) {
		return errRecycled
	}
	return err
}

// errRecycled ends a session that Recycle asked to replace.
var errRecycled = errors.New("session recycled")

// Stats returns the scale set statistics GitHub sent with the latest
// message, and when they arrived; ok is false before the first.
func (s *ScaleSet) Stats() (stats scaleset.RunnerScaleSetStatistic, at time.Time, ok bool) {
	return s.stats.latest()
}

// Recycle ends the current message session, so Run closes it and opens a new
// one at once. A fresh session gets GitHub's current statistics, and possibly
// re-offers jobs the old one never acquired. It reports whether a live session
// was there to recycle; there is none before the first or while reconnecting.
func (s *ScaleSet) Recycle(reason string) bool {
	s.mu.Lock()
	cancel := s.recycle
	s.mu.Unlock()
	if cancel == nil {
		return false
	}
	s.log.Warn("recycling the message session", "reason", reason)
	cancel(errRecycled)
	return true
}

// statsRecorder keeps the latest statistics GitHub sent.
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

func (r *statsRecorder) latest() (scaleset.RunnerScaleSetStatistic, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats, r.at, r.ok
}

// chaosExitAfterDelete reports whether FR_CHAOS_EXIT_AFTER_DELETE is set: a
// fault-injection switch for testing recovery, never for production. The
// process then exits right after GitHub accepts a message's deletion. Since
// actions/scaleset v0.5.0 a message is acted on first and deleted after, so a
// crash before the deletion has the message delivered again rather than lost.
func chaosExitAfterDelete() bool { return os.Getenv("FR_CHAOS_EXIT_AFTER_DELETE") == "1" }

// trackedSession records each successful GetMessage in the tier's health,
// logs what each message carries, and keeps its job assignments in pending:
// the library parses JobAssigned but never passes it to the Scaler.
type trackedSession struct {
	session
	health  *health.Tracker
	pending *assigned.Jobs
	offered func(jobidentity.Job)
	events  jobevent.Sink
	tier    string
	log     *slog.Logger
	// chaos exits the process after DeleteMessage (see chaosExitAfterDelete).
	chaos bool
}

func (t trackedSession) DeleteMessage(ctx context.Context, messageID int) error {
	err := t.session.DeleteMessage(ctx, messageID)
	if err == nil && t.chaos {
		fmt.Fprintf(os.Stderr, "firerunner: FR_CHAOS_EXIT_AFTER_DELETE: exiting after deleting message %d\n", messageID)
		os.Exit(137)
	}
	return err
}

// Session tells the assigned jobs how many jobs the new session starts with,
// before the library sizes the tier from that count.
func (t trackedSession) Session() scaleset.RunnerScaleSetSession {
	sess := t.session.Session()
	t.pending.Session(sess.Statistics)
	return sess
}

func (t trackedSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	m, err := t.session.GetMessage(ctx, lastMessageID, maxCapacity)
	if err == nil && t.health != nil {
		t.health.MessageOK()
	}
	if err == nil && m != nil && t.log != nil {
		t.log.Debug("message", "id", m.MessageID,
			"jobAvailable", len(m.JobAvailableMessages), "jobAssigned", len(m.JobAssignedMessages),
			"jobStarted", len(m.JobStartedMessages), "jobCompleted", len(m.JobCompletedMessages))
		for _, a := range m.JobAssignedMessages {
			if a != nil {
				t.log.Info("job assigned", jobAssignedAttrs(a.JobMessageBase)...)
			}
		}
	}
	if err == nil && m != nil {
		for _, a := range m.JobAssignedMessages {
			if a != nil {
				e := jobEvent(jobevent.Assigned, t.tier, a.JobMessageBase)
				e.At = a.ScaleSetAssignTime
				jobevent.Record(t.events, e)
			}
		}
	}
	if err == nil && m != nil && t.offered != nil {
		for _, a := range m.JobAvailableMessages {
			if a != nil {
				t.offered(offeredJob(a.JobMessageBase))
			}
		}
		for _, a := range m.JobAssignedMessages {
			if a != nil {
				t.offered(offeredJob(a.JobMessageBase))
			}
		}
	}
	if err == nil && t.pending.Message(m) && t.health != nil {
		mix := t.pending.Mix()
		t.health.SetPending(&health.Pending{Jobs: mix.Jobs, ByEvent: mix.ByEvent, Kept: mix.Kept})
	}
	return m, err
}

// offeredJob is what an offered job tells the cache's identity discovery.
func offeredJob(b scaleset.JobMessageBase) jobidentity.Job {
	return jobidentity.Job{WorkflowRef: b.JobWorkflowRef, Repository: b.OwnerName + "/" + b.RepositoryName,
		Event: b.EventName, RunID: b.WorkflowRunID, JobID: b.JobID}
}

// jobEvent fills an event from what every GitHub job message carries.
func jobEvent(kind jobevent.Kind, tier string, b scaleset.JobMessageBase) jobevent.Event {
	return jobevent.Event{
		Kind: kind, Tier: tier, JobID: b.JobID, Repository: b.OwnerName + "/" + b.RepositoryName,
		Workflow: b.JobWorkflowRef, JobName: b.JobDisplayName, EventName: b.EventName,
		RunID: b.WorkflowRunID, Labels: b.RequestLabels, QueuedAt: b.QueueTime,
	}
}

// jobAssignedAttrs are a JobAssigned message's log attributes. GitHub's
// queue and assignment times, and the delay between them, appear only when
// the message carries them: on a JIT scale set it has been seen without.
func jobAssignedAttrs(b scaleset.JobMessageBase) []any {
	attrs := []any{"repository", b.OwnerName + "/" + b.RepositoryName, "job", b.JobDisplayName,
		"event", b.EventName, "runnerRequestId", b.RunnerRequestID, "jobId", b.JobID}
	if !b.QueueTime.IsZero() {
		attrs = append(attrs, "queuedAt", b.QueueTime)
	}
	if !b.ScaleSetAssignTime.IsZero() {
		attrs = append(attrs, "assignedAt", b.ScaleSetAssignTime)
	}
	if !b.QueueTime.IsZero() && !b.ScaleSetAssignTime.IsZero() {
		attrs = append(attrs, "githubDelay", b.ScaleSetAssignTime.Sub(b.QueueTime).Round(time.Millisecond))
	}
	return attrs
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
	if s.cfg.KeepOnClose {
		s.log.Info("leaving runner scale set registered for the next start", "scaleSetID", s.scaleSetID)
		return nil
	}
	if err := s.client.DeleteRunnerScaleSet(ctx, s.scaleSetID); err != nil {
		return fmt.Errorf("delete runner scale set: %w", err)
	}
	s.log.Info("deregistered runner scale set", "scaleSetID", s.scaleSetID)
	return nil
}

// scaler adapts GitHub's scale-set callbacks onto firerunner's DesiredFunc.
// Because every microVM is ephemeral and self-terminating, job start marks the
// VM busy for shutdown drain and dispatches trusted identity resolution, and
// job completion tells the scheduler the VM's exit may re-plan for the jobs
// queued behind it.
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
	onCompleted  CompletedFunc
	events       jobevent.Sink
	tier         string
	capacity     CapacityFunc
	setCapacity  func(int)
	maxRunners   int
	log          *slog.Logger
	lastAdvert   int
	// stats keeps each message's statistics, for a long poll that ends with
	// none; acquire asks GitHub for the jobs a message offers.
	stats   *statsRecorder
	acquire func(ctx context.Context, ids []int64) ([]int64, error)
}

var _ sslistener.Scaler = (*scaler)(nil)

// Scale acts on one message as actions/scaleset v0.4.0's listener did before
// it handed messages to the Scaler: statistics, the jobs it offers, starts,
// completions, then the desired count. nil is a long poll that ended empty.
// The listener deletes the message after Scale returns, so a message Scale
// fails on, or a crash during it, is delivered again: everything here must
// bear being done twice. One known cost: a message delivered again after a
// reconnect sizes the tier from its older statistics until the next message.
func (a *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		st, _, _ := a.stats.latest()
		_, err := a.HandleDesiredRunnerCount(ctx, st.TotalAssignedJobs)
		return err
	}
	// A message is acted on in full once it is in hand, as before.
	ctx = context.WithoutCancel(ctx)
	a.stats.RecordStatistics(msg.Statistics)
	if len(msg.JobAvailableMessages) > 0 && a.acquire != nil {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			if j != nil {
				ids = append(ids, j.RunnerRequestID)
			}
		}
		a.log.Info("acquiring jobs", "count", len(ids))
		// A failure is not the message's: failing it would have the listener
		// keep it, and one that always fails would block every later message.
		// GitHub offers an unacquired job again, as when v0.4.0 lost it.
		if acquired, err := a.acquire(ctx, ids); err != nil {
			a.log.Warn("could not acquire jobs", "count", len(ids), "err", err)
		} else {
			a.log.Info("jobs acquired", "count", len(acquired))
		}
	}
	for _, j := range msg.JobStartedMessages {
		if j != nil {
			if err := a.HandleJobStarted(ctx, j); err != nil {
				return err
			}
		}
	}
	for _, j := range msg.JobCompletedMessages {
		if j != nil {
			if err := a.HandleJobCompleted(ctx, j); err != nil {
				return err
			}
		}
	}
	count := 0
	if msg.Statistics != nil {
		count = msg.Statistics.TotalAssignedJobs
	} else if st, _, ok := a.stats.latest(); ok {
		count = st.TotalAssignedJobs
	}
	_, err := a.HandleDesiredRunnerCount(ctx, count)
	return err
}

func (a *scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	// The scheduler adds its warm-runner floor to GitHub's count itself.
	running := a.onDesired(ctx, count)
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
	a.log.Info("job started", "runner", j.RunnerName, "runnerRequestId", j.RunnerRequestID,
		"repository", j.OwnerName+"/"+j.RepositoryName, "runId", j.WorkflowRunID, "jobId", j.JobID)
	e := jobEvent(jobevent.Started, a.tier, j.JobMessageBase)
	e.Runner, e.At = j.RunnerName, j.RunnerAssignTime
	jobevent.Record(a.events, e)
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
	e := jobEvent(jobevent.Completed, a.tier, j.JobMessageBase)
	e.Runner, e.Result, e.At = j.RunnerName, j.Result, j.FinishTime
	jobevent.Record(a.events, e)
	if a.onCompleted != nil && j.RunnerName != "" {
		a.onCompleted(j.RunnerName)
	}
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
// In actions/scaleset v0.4.0 a client held a mutex across every request, so a
// burst of launches sharing one generated its configs one after another: 20
// queued jobs waited up to ~20 round trips for their runners. v0.5.0 no longer
// does; whether the pool still earns its keep is to be measured. The pool grows on demand up
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

// NamePrefix is how every runner name this source generates begins.
func (j *JITSource) NamePrefix() string { return j.namePrefix + "-" }

// Owns reports whether the named runner is registered in this scale set: a
// runner of the same name prefix may belong to another install's scale set.
func (j *JITSource) Owns(ctx context.Context, name string) (bool, error) {
	opCtx, cancel := context.WithTimeout(ctx, removeOpTimeout)
	defer cancel()
	r, err := j.remover.GetRunnerByName(opCtx, name)
	if err != nil {
		return false, err
	}
	return r != nil && r.RunnerScaleSetID == j.scaleSetID, nil
}

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
