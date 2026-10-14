package listener

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/actions/scaleset"
	sslistener "github.com/actions/scaleset/listener"

	"github.com/solcreek/firerunner/internal/core"
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
	session      *scaleset.MessageSessionClient
	scaleSetID   int
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

	groupID, err := runnerGroupID(ctx, client, cfg.RunnerGroup)
	if err != nil {
		return nil, err
	}

	desired := &scaleset.RunnerScaleSet{
		Name:          cfg.Name,
		RunnerGroupID: groupID,
		Labels:        buildLabels(cfg.Name, cfg.Labels),
		// Ephemeral golden images are rebuilt out of band; the runner agent must
		// not self-update (GitHub official guidance for image-based runners).
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}

	// Get-or-create: a scale set with this name may already exist from a prior
	// (possibly unclean) run. Reuse it so restarts are idempotent instead of
	// failing with "runner scale set already exists".
	existing, err := client.GetRunnerScaleSet(ctx, groupID, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("look up runner scale set: %w", err)
	}
	var scaleSet *scaleset.RunnerScaleSet
	reused := existing != nil
	if reused {
		scaleSet, err = client.UpdateRunnerScaleSet(ctx, existing.ID, desired)
		if err != nil {
			return nil, fmt.Errorf("update runner scale set: %w", err)
		}
	} else {
		scaleSet, err = client.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return nil, fmt.Errorf("create runner scale set: %w", err)
		}
	}
	client.SetSystemInfo(systemInfo(cfg, scaleSet.ID))
	removeClient.SetSystemInfo(systemInfo(cfg, scaleSet.ID))

	session, err := openSessionWithRetry(ctx, client, scaleSet.ID, owner, cfg.Logger)
	if err != nil {
		if !reused {
			// Best-effort cleanup of the scale set we just created.
			_ = client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), scaleSet.ID)
		}
		return nil, fmt.Errorf("open message session: %w", err)
	}

	cfg.Logger.Info("registered runner scale set",
		"name", cfg.Name, "scaleSetID", scaleSet.ID, "group", cfg.RunnerGroup, "reused", reused)

	return &ScaleSet{cfg: cfg, log: cfg.Logger, client: client, removeClient: removeClient, session: session, scaleSetID: scaleSet.ID, created: !reused}, nil
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
	return &JITSource{client: s.client, remover: s.removeClient, scaleSetID: s.scaleSetID, namePrefix: s.cfg.Name}
}

// Run implements Listener: long-poll GitHub and drive onDesired.
func (s *ScaleSet) Run(ctx context.Context, onDesired DesiredFunc, onBusy BusyFunc, capacity CapacityFunc) error {
	l, err := sslistener.New(s.session, sslistener.Config{
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
		a.onJobStarted(ctx, jobidentity.Job{Runner: j.RunnerName, Repository: j.OwnerName + "/" + j.RepositoryName, Event: j.EventName, RunID: j.WorkflowRunID, JobID: j.JobID})
	}
	return nil
}

func (a *scaler) HandleJobCompleted(_ context.Context, j *scaleset.JobCompleted) error {
	a.log.Info("job completed", "runner", j.RunnerName, "result", j.Result)
	return nil
}

// JITSource generates just-in-time runner registrations against the scale set.
type JITSource struct {
	client     *scaleset.Client
	remover    runnerRemover
	scaleSetID int
	namePrefix string
}

// Generate implements scheduler.JITSource.
func (j *JITSource) Generate(ctx context.Context, _ core.RunnerSpec) (name, jitConfig string, err error) {
	name = j.namePrefix + "-" + randSuffix()
	cfg, err := j.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, j.scaleSetID)
	if err != nil {
		return "", "", fmt.Errorf("generate JIT config: %w", err)
	}
	return name, cfg.EncodedJITConfig, nil
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
