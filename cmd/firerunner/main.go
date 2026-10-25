// Command firerunner runs ephemeral Firecracker microVM GitHub Actions runners.
//
// It long-polls GitHub for queued jobs (via the official actions/scaleset
// control plane), and for each job boots a fresh, single-use microVM that
// registers a JIT/ephemeral runner, executes exactly one job, then self-
// destructs. See README.md for the architecture and security model.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/firerunnerhq/firerunner/internal/cacheserver"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/diag"
	"github.com/firerunnerhq/firerunner/internal/ec2"
	"github.com/firerunnerhq/firerunner/internal/ghrunners"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
	"github.com/firerunnerhq/firerunner/internal/listener"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/reconcile"
	"github.com/firerunnerhq/firerunner/internal/schedule"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
)

// version is overridable at build time via -ldflags; see buildVersion for
// builds that do not set it.
var version = "dev"

// buildVersion returns the -ldflags version when set, else the module
// version Go recorded (go install ...@v0.2.0 records v0.2.0), else "dev".
func buildVersion(ldflags string, info *debug.BuildInfo, ok bool) string {
	if ldflags != "dev" || !ok || info == nil {
		return ldflags
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return ldflags
}

func main() {
	info, ok := debug.ReadBuildInfo()
	version = buildVersion(version, info, ok)
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "status", "doctor":
			if err := diagnose(args[0], args[1:]); err != nil {
				os.Exit(1)
			}
			return
		case "config":
			os.Exit(configCmd(args[1:], os.Stdout, os.Stderr))
		case "cache-server":
			if err := cacheServe(args[1:]); err != nil {
				slog.Error("cache-server failed", "err", err)
				os.Exit(1)
			}
			return
		case "version", "--version", "-version":
			fmt.Println("firerunner", version)
			return
		case "help", "-h", "--help":
			usage(os.Stdout)
			return
		}
	}

	cfg, err := config.FromFlags(args)
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(2)
	}

	log := config.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("firerunner failed", "err", err)
		os.Exit(1)
	}
}

// diagnose runs the read-only "status" and "doctor" subcommands. It parses
// config leniently (config.Parse) so it can report on a partial or broken
// deployment instead of refusing to start on a missing required field.
func diagnose(cmd string, args []string) error {
	asJSON := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" || a == "-json" {
			asJSON = true
			continue
		}
		rest = append(rest, a)
	}
	cfg, err := config.Parse(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return err
	}
	switch cmd {
	case "status":
		return diag.Status(cfg, version, os.Stdout, asJSON)
	case "doctor":
		return diag.Doctor(cfg, version, os.Stdout, asJSON)
	}
	return nil
}

// cacheServe runs the self-hosted Actions dependency cache server. It binds
// 0.0.0.0 so every microVM reaches it on its own per-slot tap gateway IP;
// microVMs are pointed at it by firerunner's --cache-url/--cache-port config,
// which publishes the address into the guest via MMDS.
func cacheServe(args []string) error {
	fs, o := newCacheFlagSet()
	addr, dir, maxSize, maxEntry := o.addr, o.dir, o.maxSize, o.maxEntry
	jobsDir, jobsOwner, identityWait, artifactUpstream := o.jobsDir, o.jobsOwner, o.identityWait, o.artifactUpstream
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*jobsDir) {
		return fmt.Errorf("--jobs-dir must be an absolute path")
	}
	maxBytes, err := parseSize(*maxSize)
	if err != nil {
		return fmt.Errorf("--max-size: %w", err)
	}
	maxEntryBytes, err := parseSize(*maxEntry)
	if err != nil {
		return fmt.Errorf("--max-entry-size: %w", err)
	}
	log := config.NewLogger("info", "text")
	slog.SetDefault(log)

	srv, err := cacheserver.New(*dir, log)
	if err != nil {
		return err
	}
	srv.SetMaxSize(maxBytes)
	srv.SetMaxEntrySize(maxEntryBytes)
	reader := jobidentity.Reader{Dir: *jobsDir, OwnerUID: jobsOwner}
	if err := reader.Check(); err != nil {
		return fmt.Errorf("job identity directory: %w", err)
	}
	if err := srv.SetScope(reader.Lookup); err != nil {
		return err
	}
	srv.SetIdentityWait(*identityWait)
	if err := srv.SetArtifactUpstream(*artifactUpstream); err != nil {
		return fmt.Errorf("--artifact-upstream: %w", err)
	}
	if *artifactUpstream == "" {
		log.Warn("artifact forwarding disabled; actions/upload-artifact and download-artifact will fail on cache-redirect goldens — pass --artifact-upstream to forward them to GitHub")
	}
	stopJanitor := srv.StartJanitor()
	defer stopJanitor()

	httpSrv := &http.Server{
		Addr:        *addr,
		Handler:     srv,
		ConnContext: srv.ConnectionContext,
		// Guests run untrusted code; bound timeouts
		// keep a slow or stalled client from pinning a connection (and its
		// goroutine/fd) indefinitely. Body reads are streamed to disk with no
		// overall deadline because a large cache archive can legitimately take a
		// while, so slow-body abuse is bounded by the per-entry upload cap
		// instead; header and idle phases get hard limits here.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("cache-server listening", "addr", *addr, "dir", *dir, "max_size", *maxSize, "artifact_upstream", *artifactUpstream)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	log.Info("cache-server stopped")
	return nil
}

// cacheFlags are the cache-server subcommand's flag values.
type cacheFlags struct {
	addr, dir, maxSize, maxEntry, jobsDir, artifactUpstream *string
	jobsOwner                                               *int
	identityWait                                            *time.Duration
}

// newCacheFlagSet registers the cache-server flags.
func newCacheFlagSet() (*flag.FlagSet, cacheFlags) {
	fs := flag.NewFlagSet("cache-server", flag.ContinueOnError)
	return fs, cacheFlags{
		addr:             fs.String("addr", "127.0.0.1:8099", "listen address (bind to a guest-facing gateway IP for microVM access; source addresses identify guests; never expose it on a public interface)"),
		dir:              fs.String("dir", "/var/lib/firerunner/cache", "cache storage directory"),
		maxSize:          fs.String("max-size", "50GB", "evict least-recently-used entries above this total size (e.g. 50GB, 0 for unlimited)"),
		maxEntry:         fs.String("max-entry-size", "10GB", "refuse any single cache entry larger than this (e.g. 10GB, 0 for unlimited)"),
		jobsDir:          fs.String("jobs-dir", jobidentity.DefaultDir, "private identity directory written by the local daemon"),
		jobsOwner:        fs.Int("jobs-owner-uid", os.Getuid(), "UID of the daemon that owns job records (default current UID)"),
		identityWait:     fs.Duration("identity-wait", cacheserver.DefaultIdentityWait, "how long a request from a VM whose job identity is still pending waits for it before being denied (0 denies at once)"),
		artifactUpstream: fs.String("artifact-upstream", cacheserver.DefaultArtifactUpstream, "Results endpoint to forward ArtifactService RPCs (upload/download-artifact v4+) to, since a cache-redirect golden diverts them here along with the cache; defaults to github.com's, accepts any protocol-compatible upstream, or empty to refuse artifact RPCs"),
	}
}

// parseSize turns a human byte size ("50GB", "512MB", "1TB", or a bare byte
// count) into bytes. An empty string or "0" means unlimited (0). Both decimal
// (KB/MB/GB/TB) and binary (KiB/MiB/GiB/TiB) suffixes are accepted, case
// -insensitively; decimal suffixes use 1000, binary use 1024.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	u := strings.ToUpper(s)
	mult := int64(1)
	for _, p := range []struct {
		suf string
		m   int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	} {
		if strings.HasSuffix(u, p.suf) {
			mult = p.m
			u = strings.TrimSpace(strings.TrimSuffix(u, p.suf))
			break
		}
	}
	n, err := strconv.ParseFloat(u, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must not be negative: %q", s)
	}
	return int64(n * float64(mult)), nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `firerunner - ephemeral Firecracker microVM GitHub Actions runners

Usage:
  firerunner [flags]      run the daemon (default; see --help output of flags)
  firerunner status       show config, images, network and live microVMs
  firerunner doctor       run preflight health checks (exits non-zero on failure)
  firerunner config check validate the configuration or a tier catalog offline
                          (--catalog FILE; --json); see "firerunner config"
  firerunner cache-server run the self-hosted Actions dependency cache server
  firerunner version      print the version
  firerunner help         show this help

status and doctor read the same FR_* env / flags as the daemon, so run them
with the same environment (e.g. the systemd EnvironmentFile) as the service.
Pass --json to either for machine-readable output.

cache-server flags:
  --addr string      listen address (default "127.0.0.1:8099"; bind to a
                     guest-facing gateway IP for microVM access — never expose
                     this server on a public interface)
  --dir string       cache storage directory (default "/var/lib/firerunner/cache")
  --max-size string  evict LRU entries above this total size (default "50GB"; 0 = unlimited)
  --max-entry-size string  refuse any single entry larger than this (default "10GB"; 0 = unlimited)
  --jobs-owner-uid int  daemon UID that owns the records (default current UID)
  --jobs-dir string  daemon's private job identity directory (default "/run/firerunner/jobs")
  --artifact-upstream string  forward ArtifactService RPCs (upload/download-
                     artifact v4+, which share ACTIONS_RESULTS_URL with the
                     cache) to this Results endpoint so artifacts stay on GitHub
                     (default github.com's results-receiver; "" refuses them)
`)
}

// tierProvisioner returns the provisioner that launches t's runners. fc is the
// host's Firecracker provisioner, shared by every Firecracker tier because they
// draw on one slot budget; it is nil when no tier uses Firecracker. newEC2
// builds an ec2 tier's own provisioner; it is nil when no tier uses EC2.
func tierProvisioner(t config.Tier, fc provisioner.Provisioner, newEC2 func(config.Tier) provisioner.Provisioner) (provisioner.Provisioner, error) {
	switch t.ProvisionerName() {
	case config.ProvisionerFirecracker:
		if fc == nil {
			return nil, errors.New("no firecracker provisioner configured")
		}
		return fc, nil
	case config.ProvisionerEC2:
		if newEC2 == nil || t.EC2 == nil {
			return nil, errors.New("no ec2 provisioner configured")
		}
		return newEC2(t), nil
	default:
		return nil, fmt.Errorf("unknown provisioner %q", t.Provisioner)
	}
}

// serveTiers runs every tier's listener until ctx ends. Each run gets its own
// child context, cancelled once that run returns, so a tier that stops also
// stops scheduling: its idle runners are cancelled and its warm pool no
// longer refills, freeing shared slots and instances for the tiers still
// serving. A tier whose listener stops on its own error is logged and left
// stopped (its health tracker says why) while the others keep serving; when
// every tier has stopped there is nothing left to serve, so it cancels ctx and
// returns the first error. A tier that needs a fresh process
// (listener.ErrRestart: its scale set was deleted) stops every tier at once,
// so the daemon exits for its supervisor to restart and re-register it.
func serveTiers(ctx context.Context, cancel context.CancelFunc, names []string, runs []func(context.Context) error, log *slog.Logger) error {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		stopped int
		first   error
		restart bool
	)
	for i, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tierCtx, tierCancel := context.WithCancel(ctx)
			defer tierCancel()
			err := run(tierCtx)
			tierCancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("listener returned without an error")
			}
			mu.Lock()
			defer mu.Unlock()
			stopped++
			if first == nil || errors.Is(err, listener.ErrRestart) {
				first = fmt.Errorf("tier %q: %w", names[i], err)
			}
			switch {
			case errors.Is(err, listener.ErrRestart):
				log.Error("restarting to register the scale set again; stopping every tier", "tier", names[i], "err", err)
				restart = true
				cancel()
			case stopped == len(runs):
				log.Error("tier stopped taking jobs", "tier", names[i], "err", err)
				log.Error("every tier has stopped; shutting down")
				cancel()
			default:
				log.Error("tier stopped taking jobs; other tiers keep running", "tier", names[i], "err", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if restart || stopped == len(runs) {
		return first
	}
	return nil
}

// reaperOptions configures the EC2 reaper for this install: which tiers keep
// a pool, how to deregister each tier's runners, and the lifetime backstop.
func reaperOptions(cfg *config.Config, owner string, tiers []config.Tier, listeners []*listener.ScaleSet, log *slog.Logger) ec2.ReaperOptions {
	removers := map[string]*listener.JITSource{}
	for i, t := range tiers {
		if t.IsEC2() {
			removers[t.Name] = listeners[i].JIT()
		}
	}
	return ec2.ReaperOptions{
		Owner:       owner,
		PooledTiers: pooledTiers(tiers),
		Remove: func(ctx context.Context, tier, runner string) error {
			jit, ok := removers[tier]
			if !ok {
				// A tier no longer configured: its runner cannot be asked
				// about; the lifetime backstop ends the instance.
				return fmt.Errorf("tier %q is not configured", tier)
			}
			return jit.RemoveRunner(ctx, runner)
		},
		MaxLifetime: cfg.Firecracker.MaxVMLifetime,
		DryRun:      cfg.EC2Reaper == "dry-run",
		Logger:      log,
	}
}

// scheduleTarget is what a tier's schedule resizes: its stopped pool, if it
// keeps one, then the hot runners the scheduler keeps beside GitHub's jobs.
func scheduleTarget(sched *scheduler.Scheduler, prov provisioner.Provisioner, tracker *health.Tracker) schedule.Target {
	t := schedule.Target{
		SetMin: sched.SetMin,
		Applied: func(s schedule.Sizes) {
			tracker.SetSizes(health.Sizes{Min: s.Min, Pool: s.Pool, Window: s.Window, Since: time.Now()})
		},
	}
	if p, ok := prov.(interface{ Pool() *ec2.Pool }); ok && p.Pool() != nil {
		t.SetPool = p.Pool().SetTarget
	}
	return t
}

// pooledTiers names the EC2 tiers that keep a stopped pool at some time of
// their schedule: the reaper leaves their ready and warming instances to the
// pool, which trims itself when its window closes.
func pooledTiers(tiers []config.Tier) map[string]bool {
	pooled := map[string]bool{}
	for _, t := range tiers {
		if t.IsEC2() && t.MaxPool() > 0 {
			pooled[t.Name] = true
		}
	}
	return pooled
}

// offlineRunners lists a tier's offline, idle runners for the reconciler:
// those named for its scale set that GitHub confirms belong to it.
type offlineRunners struct {
	list interface {
		Runners(context.Context) ([]ghrunners.Runner, error)
	}
	jit interface {
		NamePrefix() string
		Owns(ctx context.Context, name string) (bool, error)
		RemoveRunner(ctx context.Context, name string) error
	}
}

func (o offlineRunners) Offline(ctx context.Context) ([]string, error) {
	rs, err := o.list.Runners(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rs {
		if r.Status != "offline" || r.Busy || !strings.HasPrefix(r.Name, o.jit.NamePrefix()) {
			continue
		}
		// Another install's scale set may use the same name prefix.
		if ok, err := o.jit.Owns(ctx, r.Name); err != nil || !ok {
			continue
		}
		names = append(names, r.Name)
	}
	return names, nil
}

func (o offlineRunners) Remove(ctx context.Context, name string) error {
	return o.jit.RemoveRunner(ctx, name)
}

// statsSource adapts a listener to the reconciler.
type statsSource struct{ lis *listener.ScaleSet }

func (s statsSource) Stats() (reconcile.Stats, time.Time, bool) {
	st, at, ok := s.lis.Stats()
	return reconcile.Stats{
		Available: st.TotalAvailableJobs, Acquired: st.TotalAcquiredJobs, Assigned: st.TotalAssignedJobs,
		Running: st.TotalRunningJobs, Registered: st.TotalRegisteredRunners, Busy: st.TotalBusyRunners,
		Idle: st.TotalIdleRunners,
	}, at, ok
}

func (s statsSource) Recycle(reason string) bool { return s.lis.Recycle(reason) }

// tierPending returns the pending-launch counter a tier's scheduler shares.
// Firecracker tiers share one, because their launches draw on the same host
// slots. An EC2 tier draws on no host slots, so it gets nil (a private
// counter): its launches must not shrink the capacity Firecracker tiers
// advertise.
func tierPending(t config.Tier, shared *scheduler.PendingLaunches) *scheduler.PendingLaunches {
	if t.IsEC2() {
		return nil
	}
	return shared
}

func tierNames(tiers []config.Tier) []string {
	names := make([]string, len(tiers))
	for i, t := range tiers {
		names[i] = t.Name
	}
	return names
}

// healthInterval is how often the listener health file is rewritten.
const healthInterval = 10 * time.Second

// publishHealth writes the trackers' state, and the reaper's when reaper is
// non-nil, to path now and every healthInterval until the returned func is
// called, which writes it once more.
func publishHealth(path string, trackers []*health.Tracker, reaper func() *health.Reaper, log *slog.Logger) func() {
	write := func() {
		f := health.File{PID: os.Getpid(), WrittenAt: time.Now().UTC()}
		for _, tr := range trackers {
			f.Tiers = append(f.Tiers, tr.Snapshot())
		}
		if reaper != nil {
			f.Reaper = reaper()
		}
		if err := health.Write(path, f); err != nil {
			log.Warn("write listener health", "path", path, "err", err)
		}
	}
	write()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(healthInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				write()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		write()
	}
}

// serveJIT starts the endpoint EC2 instances fetch their JIT configs from, on
// the control VM's VPC address. Binding fails fast if the address is not this
// host's. The returned func shuts it down.
func serveJIT(ctx context.Context, cfg *config.Config, log *slog.Logger) (*jitserver.Server, func(), error) {
	jit := jitserver.New(cfg.JITTTL, log.With("component", "jitserver"))
	ln, err := net.Listen("tcp", cfg.JITAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("--jit-addr: %w", err)
	}
	srv := &http.Server{Handler: jit, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("jit endpoint stopped", "err", err)
		}
	}()
	log.Info("serving EC2 JIT configs", "addr", cfg.JITAddr, "url", jitserver.URL(cfg.JITEndpoint()))
	return jit, func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}, nil
}

// newEC2Client builds the AWS clients shared by every EC2 tier from the
// default credential chain (the control VM's instance role). The region is
// --aws-region when set, else the SDK's usual sources, falling back to IMDS.
func newEC2Client(ctx context.Context, cfg *config.Config, log *slog.Logger) (*ec2.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithEC2IMDSRegion()}
	if cfg.AWSRegion != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("no AWS region: set --aws-region or run on EC2")
	}
	log.Info("EC2 provisioner ready", "region", awsCfg.Region)
	return ec2.NewClient(awsec2.NewFromConfig(awsCfg), awsssm.NewFromConfig(awsCfg), 0, log.With("component", "ec2")), nil
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	privateKey, err := cfg.ResolvePrivateKey()
	if err != nil {
		return err
	}

	// The owner identifies this install's instances across restarts, and
	// across replacements of the host when --owner is set.
	owner := cfg.Owner
	if owner == "" {
		if owner, err = os.Hostname(); err != nil || owner == "" {
			owner = "firerunner"
		}
	}

	app := scaleset.GitHubAppAuth{
		ClientID:       cfg.AppClientID,
		InstallationID: cfg.AppInstallID,
		PrivateKey:     privateKey,
	}

	// The REST runner list behind --runner-sweep, shared by every tier.
	var runnerList *ghrunners.Client
	if cfg.RunnerSweep != "off" {
		if runnerList, err = ghrunners.New(cfg.URL, cfg.Token, app); err != nil {
			log.Warn("runner sweep disabled", "err", err)
			runnerList = nil
		}
	}

	// Each tier is its own GitHub scale set; developers pick one with
	// runs-on: <tier name>. One Firecracker provisioner backs every Firecracker
	// tier: they share the host kernel, tool cache, network and the MaxRunners
	// slot budget, while each keeps its own microVM shape and golden image.

	var onJobStarted func(context.Context, jobidentity.Job)
	if cfg.Firecracker.CachePort != 0 || cfg.Firecracker.CacheURL != "" {
		jobs, err := jobidentity.NewRegistry(cfg.CacheJobsDir, log)
		if err != nil {
			return fmt.Errorf("cache job registry: %w", err)
		}
		defer jobs.Close()
		resolver, err := jobidentity.NewGitHub(cfg.URL, cfg.Token, app)
		if err != nil {
			return err
		}
		cfg.Firecracker.Jobs = jobs
		onJobStarted = func(ctx context.Context, j jobidentity.Job) { jobs.Start(ctx, j, resolver) }
	}
	var fc *provisioner.Firecracker
	if cfg.UsesFirecracker() {
		fc = provisioner.NewFirecracker(cfg.Firecracker, log)
	}
	var (
		newEC2    func(config.Tier) provisioner.Provisioner
		ec2Client *ec2.Client
	)
	if cfg.UsesEC2() {
		jit, stopJIT, err := serveJIT(ctx, cfg, log)
		if err != nil {
			return err
		}
		// Instances still running their jobs fetch nothing more, but stop the
		// endpoint only after the drain all the same.
		defer stopJIT()
		client, err := newEC2Client(ctx, cfg, log)
		if err != nil {
			return err
		}
		ec2Client = client
		newEC2 = func(t config.Tier) provisioner.Provisioner {
			// A scheduled pool starts at the current window's size; the
			// schedule follower resizes it from there.
			e := *t.EC2
			e.Pool = t.SizesAt(time.Now()).Pool
			p := client.Tier(ec2.Options{
				Tier:        t.Name,
				Owner:       owner,
				EC2:         e,
				Pooled:      t.MaxPool() > 0,
				JIT:         jit,
				JITURL:      jitserver.URL(cfg.JITEndpoint()),
				MaxLifetime: cfg.Firecracker.MaxVMLifetime,
				Logger:      log.With("tier", t.Name),
			})
			// A tier's stopped pool fills and refills in the background for
			// the life of the process; its instances stay stopped on exit and
			// are found again on the next start.
			if pool := p.Pool(); pool != nil {
				go pool.Run(ctx)
			}
			return p
		}
	}
	tiers := cfg.EffectiveTiers()
	provs := make([]provisioner.Provisioner, len(tiers))
	for i, t := range tiers {
		var shared provisioner.Provisioner
		if fc != nil {
			shared = fc
		}
		p, err := tierProvisioner(t, shared, newEC2)
		if err != nil {
			return fmt.Errorf("tier %q: %w", t.Name, err)
		}
		provs[i] = p
	}

	// Register every tier's scale set up front so a failure aborts cleanly
	// before we start booting microVMs.
	var (
		listeners []*listener.ScaleSet
		scheds    []*scheduler.Scheduler
		trackers  []*health.Tracker
		pending   scheduler.PendingLaunches
	)
	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close(context.WithoutCancel(ctx))
		}
	}
	for i, t := range tiers {
		tlog := log.With("tier", t.Name)
		tracker := health.NewTracker(t.Name)
		trackers = append(trackers, tracker)
		lis, err := listener.New(ctx, listener.Config{
			URL:          cfg.URL,
			OnJobStarted: onJobStarted,
			Health:       tracker,
			Name:         t.Name,
			RunnerGroup:  cfg.RunnerGroup,
			Labels:       t.Labels,
			MaxRunners:   t.Max,
			Token:        cfg.Token,
			App:          app,
			Version:      version,
			Logger:       tlog,
		}, owner)
		if err != nil {
			closeAll()
			return fmt.Errorf("tier %q: %w", t.Name, err)
		}
		listeners = append(listeners, lis)
		jit := lis.JIT()
		scheds = append(scheds, scheduler.New(scheduler.Options{
			Max:         t.Max,
			Min:         t.SizesAt(time.Now()).Min,
			Spec:        t.Spec(),
			Provisioner: provs[i],
			JIT:         jit,
			Remover:     jit,
			Logger:      tlog,
			Pending:     tierPending(t, &pending),
		}))
	}
	defer closeAll()

	var reaperState func() *health.Reaper
	if ec2Client != nil && cfg.EC2Reaper != "off" {
		reaper := ec2Client.NewReaper(reaperOptions(cfg, owner, tiers, listeners, log))
		go reaper.Run(ctx)
		reaperState = func() *health.Reaper {
			rep, ok := reaper.Last()
			if !ok {
				return nil
			}
			return &health.Reaper{
				At: rep.At, DryRun: rep.DryRun, Instances: rep.Instances, SpotRequests: rep.SpotRequests,
				Busy: rep.Busy, Young: rep.Young, Error: rep.Err,
			}
		}
	}

	log.Info("firerunner starting",
		"url", cfg.URL,
		"tiers", len(tiers),
		"maxRunners", cfg.MaxRunners,
	)
	for i, t := range tiers {
		if t.IsEC2() {
			log.Info("serving tier",
				"name", t.Name, "provisioner", provs[i].Name(),
				"instanceTypes", t.EC2.InstanceTypes, "market", t.EC2.Market,
				"min", t.Min, "max", t.Max)
			continue
		}
		log.Info("serving tier",
			"name", t.Name, "provisioner", provs[i].Name(),
			"vcpu", t.VCPU, "memMiB", t.MemMiB,
			"golden", t.Golden, "min", t.Min, "max", t.Max)
	}

	if fc != nil {
		// Reclaim taps and job dirs orphaned by a previous unclean exit before
		// we start allocating new slots, so a crash-restart cycle can't exhaust
		// them.
		fc.CleanupStale(ctx)

		if err := fc.SetupNetwork(ctx); err != nil {
			return fmt.Errorf("setup network: %w", err)
		}
		go fc.RefreshNetwork(ctx)
	}

	// Publish each tier's listener health for status and doctor.
	healthPath := filepath.Join(cfg.Firecracker.WorkDir, health.FileName)
	stopHealth := publishHealth(healthPath, trackers, reaperState, log)

	// Drive every tier's listener concurrently. A listener reconnects its own
	// session; one that stops on a fatal error leaves the other tiers running.
	// runCtx ends on SIGTERM, or once every tier has stopped. microVMs and the
	// warm-pool top-up bind to runCtx too, so either way listeners stop,
	// Reconcile no-ops, and Drain settles.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runs := make([]func(context.Context) error, len(listeners))
	for i := range listeners {
		lis, sched, tracker, prov := listeners[i], scheds[i], trackers[i], provs[i]
		// tierCtx is this tier's own lifetime (see serveTiers): launches, the
		// re-plan after a runner exits and the warm-pool top-up bind to it,
		// so a tier that stops cancels its idle runners and stops launching,
		// while busy ones finish.
		runs[i] = func(tierCtx context.Context) error {
			onDesired := func(_ context.Context, desired int) int {
				sched.Reconcile(tierCtx, desired)
				return sched.Running()
			}
			onBusy := func(name string) { sched.MarkBusy(name) }
			onCompleted := func(name string) { sched.MarkCompleted(name) }
			capacity := func() int { return sched.Capacity() }
			// The reconciler compares GitHub's statistics with this tier's
			// runners for as long as the tier serves.
			jit := lis.JIT()
			ropts := reconcile.Options{
				Health: tracker, Logger: log.With("tier", tiers[i].Name, "component", "reconcile"),
			}
			if runnerList != nil {
				ropts.Offline = offlineRunners{list: runnerList, jit: jit}
				ropts.OfflineDryRun = cfg.RunnerSweep == "dry-run"
			}
			go reconcile.New(statsSource{lis}, sched, jit.Exists, ropts).Run(tierCtx)
			if tiers[i].Schedule != nil {
				go schedule.Follow(tierCtx, tiers[i].SizesAt, scheduleTarget(sched, prov, tracker),
					30*time.Second, nil, log.With("tier", tiers[i].Name, "component", "schedule"))
			}
			return lis.Run(tierCtx, onDesired, onBusy, onCompleted, capacity)
		}
	}
	runErr := serveTiers(runCtx, cancel, tierNames(tiers), runs, log)
	// No tier is listening any more. Stop refreshing the health file before
	// the drain, which can wait out long jobs: a file that keeps its last
	// write time ages into "stale" instead of claiming live listeners.
	stopHealth()

	log.Info("shutdown signalled, draining in-flight microVMs")
	for _, sched := range scheds {
		sched.Drain()
	}
	log.Info("firerunner stopped")

	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}
