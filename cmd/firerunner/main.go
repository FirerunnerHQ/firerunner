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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	awssq "github.com/aws/aws-sdk-go-v2/service/servicequotas"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/awscreds"
	"github.com/firerunnerhq/firerunner/internal/cacheserver"
	"github.com/firerunnerhq/firerunner/internal/cacheserver/s3store"
	"github.com/firerunnerhq/firerunner/internal/cachetls"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/diag"
	"github.com/firerunnerhq/firerunner/internal/ec2"
	"github.com/firerunnerhq/firerunner/internal/ghauth"
	"github.com/firerunnerhq/firerunner/internal/ghrunners"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
	"github.com/firerunnerhq/firerunner/internal/listener"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/reconcile"
	"github.com/firerunnerhq/firerunner/internal/schedule"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
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
		case "migrate":
			os.Exit(migrateCmd(args[1:], os.Stdout, os.Stderr))
		case "cache-server":
			if err := cacheServe(args[1:]); err != nil {
				slog.Error("cache-server failed", "err", err)
				os.Exit(1)
			}
			return
		case "git-proxy":
			if err := runGitProxy(args[1:]); err != nil {
				slog.Error("git-proxy failed", "err", err)
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
	args, err := diagEnv(cmd, args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return err
	}
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
		return diag.Doctor(cfg, version, os.Stdout, asJSON, ec2Preflight(cfg)...)
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
	maxEntries, maxRepoEntries := o.maxEntries, o.maxRepoEntries
	jobsDir, jobsOwner, identityWait, artifactUpstream := o.jobsDir, o.jobsOwner, o.identityWait, o.artifactUpstream
	s3Bucket, s3Prefix, s3Region := o.s3Bucket, o.s3Prefix, o.s3Region
	s3LocalCache, s3CredsFile := o.s3LocalCache, o.s3CredsFile
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *s3CredsFile != "" && *s3Bucket == "" {
		return fmt.Errorf("--s3-credentials-file needs --s3-bucket")
	}
	if !filepath.IsAbs(*jobsDir) {
		return fmt.Errorf("--jobs-dir must be an absolute path")
	}
	if *o.bodyIdle < 0 {
		return fmt.Errorf("--body-idle-timeout must be >= 0 (0 = no bound)")
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
	if *maxEntries < 0 || *maxRepoEntries < 0 {
		return fmt.Errorf("--max-entries and --max-repo-entries must be >= 0 (0 = unlimited)")
	}
	srv.SetMaxEntries(*maxEntries, *maxRepoEntries)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *s3Bucket != "" {
		creds, err := s3Credentials(ctx, *s3CredsFile, log)
		if err != nil {
			return err
		}
		store, err := s3store.New(ctx, *s3Bucket, *s3Prefix, *s3Region, creds)
		if err != nil {
			return err
		}
		localBytes, err := parseSize(*s3LocalCache)
		if err != nil {
			return fmt.Errorf("--s3-local-cache-size: %w", err)
		}
		srv.SetLocalCacheSize(localBytes)
		if err := srv.SetBlobStore(store); err != nil {
			return err
		}
		stopSync := srv.StartIndexSync()
		defer stopSync()
	}
	reader := jobidentity.Reader{Dir: *jobsDir, OwnerUID: jobsOwner}
	if err := reader.Check(); err != nil {
		return fmt.Errorf("job identity directory: %w", err)
	}
	if err := srv.SetScope(reader.Lookup); err != nil {
		return err
	}
	srv.SetIdentityWait(*identityWait)
	srv.SetBodyIdle(*o.bodyIdle)
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
		// while; instead a body that stalls is cut after --body-idle-timeout
		// (srv.SetBodyIdle), and header and idle phases get hard limits here.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("cache-server listening", "addr", *addr, "dir", *dir, "max_size", *maxSize, "artifact_upstream", *artifactUpstream, "s3_bucket", *s3Bucket, "s3_prefix", *s3Prefix)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	// ListenAndServe returns as Shutdown starts. Wait for the requests in
	// flight, so the deferred index sync pushes the entries they finalize.
	<-drained
	log.Info("cache-server stopped")
	return nil
}

// s3Credentials is what the S3 store signs with: the credentials in file,
// read again before they expire, or nil for the default AWS chain when file
// is empty. It waits for the file to hold credentials, as the daemon that
// writes it may start after cache-server; only ctx ends the wait.
func s3Credentials(ctx context.Context, file string, log *slog.Logger) (aws.CredentialsProvider, error) {
	if file == "" {
		return nil, nil
	}
	if !filepath.IsAbs(file) {
		return nil, fmt.Errorf("--s3-credentials-file must be an absolute path")
	}
	if err := awscreds.Wait(ctx, file, time.Second, log); err != nil {
		return nil, fmt.Errorf("--s3-credentials-file: %w", err)
	}
	return awscreds.NewFileCache(file), nil
}

// cacheFlags are the cache-server subcommand's flag values.
type cacheFlags struct {
	addr, dir, maxSize, maxEntry, jobsDir, artifactUpstream *string
	s3Bucket, s3Prefix, s3Region, s3LocalCache, s3CredsFile *string
	jobsOwner, maxEntries, maxRepoEntries                   *int
	identityWait, bodyIdle                                  *time.Duration
}

// newCacheFlagSet registers the cache-server flags.
func newCacheFlagSet() (*flag.FlagSet, cacheFlags) {
	fs := flag.NewFlagSet("cache-server", flag.ContinueOnError)
	return fs, cacheFlags{
		addr:             fs.String("addr", "127.0.0.1:8099", "listen address (bind to a guest-facing gateway IP for microVM access; source addresses identify guests; never expose it on a public interface)"),
		dir:              fs.String("dir", "/var/lib/firerunner/cache", "cache storage directory"),
		maxSize:          fs.String("max-size", "50GB", "evict least-recently-used entries above this total size (e.g. 50GB, 0 for unlimited)"),
		maxEntry:         fs.String("max-entry-size", "10GB", "refuse any single cache entry larger than this (e.g. 10GB, 0 for unlimited)"),
		maxEntries:       fs.Int("max-entries", cacheserver.DefaultMaxEntries, "keep at most this many entries, completed and reserved, evicting least-recently-used ones first (0 for unlimited)"),
		maxRepoEntries:   fs.Int("max-repo-entries", cacheserver.DefaultMaxRepoEntries, "keep at most this many entries for one repository, evicting its own least-recently-used ones first (0 for unlimited)"),
		jobsDir:          fs.String("jobs-dir", jobidentity.DefaultDir, "private identity directory written by the local daemon"),
		jobsOwner:        fs.Int("jobs-owner-uid", os.Getuid(), "UID of the daemon that owns job records (default current UID)"),
		identityWait:     fs.Duration("identity-wait", cacheserver.DefaultIdentityWait, "how long a request from a VM whose job identity is still pending waits for it before being denied (0 denies at once)"),
		bodyIdle:         fs.Duration("body-idle-timeout", cacheserver.DefaultBodyIdle, "end a request whose body sends nothing for this long; an upload that keeps sending is never cut (0 for no bound)"),
		artifactUpstream: fs.String("artifact-upstream", cacheserver.DefaultArtifactUpstream, "Results endpoint to forward ArtifactService RPCs (upload/download-artifact v4+) to, since a cache-redirect golden diverts them here along with the cache; defaults to github.com's, accepts any protocol-compatible upstream, or empty to refuse artifact RPCs"),
		s3Bucket:         fs.String("s3-bucket", "", "keep completed blobs and the index in this S3 bucket instead of --dir, which then only stages uploads; credentials come from the default AWS chain, or --s3-credentials-file"),
		s3CredsFile:      fs.String("s3-credentials-file", "", "with --s3-bucket, sign with the credentials in this file (credential_process JSON, kept fresh by the daemon's --cache-role-arn) instead of the default AWS chain, waiting for it at start; pass --s3-region too, as instance metadata may be unreachable"),
		s3Prefix:         fs.String("s3-prefix", "firerunner-cache/", "key prefix inside --s3-bucket; one live cache-server per prefix"),
		s3Region:         fs.String("s3-region", "", "region of --s3-bucket (default: from the AWS config or instance metadata)"),
		s3LocalCache:     fs.String("s3-local-cache-size", "20GB", "with --s3-bucket, keep up to this much of the most recently used blobs on local disk so restores skip S3 (0 keeps none)"),
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
  firerunner git-proxy    run an EC2 instance's git checkout cache proxy
  firerunner migrate      open a pull request that moves a repository's jobs onto
                          firerunner (runs-on labels); see "firerunner migrate -h"
  firerunner version      print the version
  firerunner help         show this help

status and doctor read the same FR_* env / flags as the daemon. With no FR_*
variable set they read the daemon's /etc/firerunner/firerunner.env and say
so; --env-file FILE reads another (a jailer instance's), --env-file none
none. Pass --json to either for machine-readable output.

cache-server flags:
  --addr string      listen address (default "127.0.0.1:8099"; bind to a
                     guest-facing gateway IP for microVM access — never expose
                     this server on a public interface)
  --dir string       cache storage directory (default "/var/lib/firerunner/cache")
  --max-size string  evict LRU entries above this total size (default "50GB"; 0 = unlimited)
  --max-entry-size string  refuse any single entry larger than this (default "10GB"; 0 = unlimited)
  --max-entries int  keep at most this many entries, evicting LRU ones first (default 20000; 0 = unlimited)
  --max-repo-entries int  the same for one repository, evicting its own first (default 2000; 0 = unlimited)
  --body-idle-timeout duration  end a request whose body sends nothing this long
                     (default 60s; 0 = no bound)
  --jobs-owner-uid int  daemon UID that owns the records (default current UID)
  --jobs-dir string  daemon's private job identity directory (default "/run/firerunner/jobs")
  --artifact-upstream string  forward ArtifactService RPCs (upload/download-
                     artifact v4+, which share ACTIONS_RESULTS_URL with the
                     cache) to this Results endpoint so artifacts stay on GitHub
                     (default github.com's results-receiver; "" refuses them)
  --s3-bucket string  keep completed blobs and the index in this S3 bucket;
                     --dir then only stages uploads (credentials: default AWS chain)
  --s3-credentials-file string  sign S3 requests with the credentials in this
                     file, which the daemon's --cache-role-arn keeps fresh,
                     instead of the default AWS chain; waits for it at start
  --s3-prefix string  key prefix in --s3-bucket (default "firerunner-cache/");
                     one live cache-server per prefix
  --s3-region string  region of --s3-bucket (default from AWS config / IMDS)
  --s3-local-cache-size string  keep this much of the most recently used blobs
                     on local disk in front of S3 (default "20GB"; 0 = none)
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

// poolPolicy is the tier's ec2.pool_for as an assigned-jobs policy; nil, every
// launch may take the pool.
func poolPolicy(t config.Tier) *assigned.Policy {
	if t.EC2 == nil || t.EC2.PoolFor == nil {
		return nil
	}
	return &assigned.Policy{Events: t.EC2.PoolFor.Events, Jobs: t.EC2.PoolFor.Jobs}
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

// offlineRunners lists a tier's offline, idle runners for the reconciler,
// by name; Remove checks that one is this scale set's before removing it.
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
		if r.Status == "offline" && !r.Busy && strings.HasPrefix(r.Name, o.jit.NamePrefix()) {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

// Remove asks the scale-set API whose runner it is only now, at removal:
// asking for every offline runner on every sweep would load the client
// scale-down removals use.
func (o offlineRunners) Remove(ctx context.Context, name string) error {
	ours, err := o.jit.Owns(ctx, name)
	switch {
	case err != nil:
		return err
	case !ours:
		return reconcile.ErrNotOurs
	}
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

// healthBreakers converts the EC2 launch options backing off for the health
// file.
func healthBreakers(bs []ec2.Breaker) []health.Breaker {
	var out []health.Breaker
	for _, b := range bs {
		out = append(out, health.Breaker{
			Market: b.Market, Type: b.Type, Subnet: b.Subnet, Reason: b.Reason,
			Failures: b.Failures, Since: b.Since.UTC(), Until: b.Until.UTC(),
		})
	}
	return out
}

// healthCacheCredentials is the refresher's state for the health file, nil
// without one.
func healthCacheCredentials(r *awscreds.Refresher) *health.CacheCredentials {
	if r == nil {
		return nil
	}
	st := r.Status()
	return &health.CacheCredentials{
		File: st.File, LastRefresh: st.LastRefresh, Expires: st.Expires,
		LastError: st.LastError, LastErrorAt: st.LastErrorAt,
	}
}

// healthInterval is how often the listener health file is rewritten.
const healthInterval = 10 * time.Second

// publishHealth writes the trackers' state, with whatever fill adds (the
// reaper's, the EC2 breakers) when fill is non-nil, to path now and every
// healthInterval until the returned func is called, which writes it once more.
func publishHealth(path string, trackers func() []*health.Tracker, fill func(*health.File), log *slog.Logger) func() {
	write := func() {
		f := health.File{PID: os.Getpid(), WrittenAt: time.Now().UTC()}
		for _, tr := range trackers() {
			f.Tiers = append(f.Tiers, tr.Snapshot())
		}
		if fill != nil {
			fill(&f)
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

// loadCacheCAs returns the results host's CA and, when a tier has a git cache,
// the CA for github.com. A CA nothing needs is not created.
func loadCacheCAs(cfg *config.Config) (ca, git *cachetls.CA, err error) {
	ca, err = cachetls.LoadOrCreate(filepath.Join(cfg.Firecracker.WorkDir, "cache-tls"))
	if err != nil {
		return nil, nil, fmt.Errorf("cache CA: %w", err)
	}
	if cfg.UsesEC2GitCache() {
		git, err = cachetls.LoadOrCreateFor(filepath.Join(cfg.Firecracker.WorkDir, "git-tls"), cachetls.GitHost)
		if err != nil {
			return nil, nil, fmt.Errorf("git cache CA: %w", err)
		}
	}
	return ca, git, nil
}

// firecrackerCache reports whether microVMs use a local cache-server.
func firecrackerCache(cfg *config.Config) bool {
	return cfg.Firecracker.CachePort != 0 || cfg.Firecracker.CacheURL != ""
}

// leafTTL is how long an EC2 instance's cache leaf must live: through the
// wait for its config, its boot, and the longest the backstop lets it run
// (a day when that backstop is off).
func leafTTL(cfg *config.Config) time.Duration {
	life := cfg.Firecracker.MaxVMLifetime
	if life <= 0 {
		life = 24 * time.Hour
	}
	return cfg.JITTTL + 15*time.Minute + life
}

// ec2Preflight runs the EC2 account checks for doctor when a tier launches
// instances; none otherwise.
func ec2Preflight(cfg *config.Config) []diag.Check {
	if !cfg.UsesEC2() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return []diag.Check{{Level: "FAIL", Name: "aws-identity", Detail: err.Error()}}
	}
	ec2API := awsec2.NewFromConfig(awsCfg)
	client := ec2.NewClient(ec2API, awsssm.NewFromConfig(awsCfg), 0, nil)
	var out []diag.Check
	for _, f := range client.Preflight(ctx, awsCfg.Region, ec2API, awssq.NewFromConfig(awsCfg), awssts.NewFromConfig(awsCfg), cfg.EffectiveTiers()) {
		out = append(out, diag.Check{Level: f.Level, Name: f.Name, Detail: f.Detail})
	}
	return out
}

// newAppJWT returns the App's JWT source, for every tenant and client in the
// process: its key from --app-private-key, or AWS KMS, where the key never
// leaves the service. Nil for a PAT install.
func newAppJWT(ctx context.Context, cfg *config.Config, privateKey string) (*ghauth.JWT, error) {
	switch {
	case cfg.AppClientID == "":
		return nil, nil
	case cfg.AppKMSKey != "":
		client, err := kmsClient(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return ghauth.NewJWT(cfg.AppClientID, ghauth.KMSSigner{API: client, KeyID: cfg.AppKMSKey}), nil
	case privateKey != "":
		key, err := config.ParseAppPrivateKey([]byte(privateKey))
		if err != nil {
			return nil, fmt.Errorf("GitHub App private key: %w", err)
		}
		return ghauth.NewJWT(cfg.AppClientID, ghauth.KeySigner{Key: key}), nil
	}
	return nil, nil
}

// kmsClient is a KMS client in the region of --app-kms-key, which an ARN names
// and need not be the VM's: a client for another region cannot find the key.
func kmsClient(ctx context.Context, cfg *config.Config) (*kms.Client, error) {
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("--app-kms-key: %w", err)
	}
	if r := ghauth.KeyRegion(cfg.AppKMSKey); r != "" {
		awsCfg.Region = r
	}
	return kms.NewFromConfig(awsCfg), nil
}

// loadAWSConfig loads the default credential chain (the control VM's
// instance role). The region is --aws-region when set, else the SDK's usual
// sources, falling back to IMDS.
func loadAWSConfig(ctx context.Context, cfg *config.Config) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithEC2IMDSRegion()}
	if cfg.AWSRegion != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		return aws.Config{}, errors.New("no AWS region: set --aws-region or run on EC2")
	}
	return awsCfg, nil
}

// newEC2Client builds the AWS clients shared by every EC2 tier from the
// default credential chain (the control VM's instance role). The region is
// --aws-region when set, else the SDK's usual sources, falling back to IMDS.
func newEC2Client(ctx context.Context, cfg *config.Config, log *slog.Logger) (*ec2.Client, error) {
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	log.Info("EC2 provisioner ready", "region", awsCfg.Region)
	return ec2.NewClient(awsec2.NewFromConfig(awsCfg), awsssm.NewFromConfig(awsCfg), 0, log.With("component", "ec2")), nil
}

// openJobEvents returns where job events go, and how to close it. The store
// connects in the background, so a database that is down costs the record, never
// a job, nor any time at start.
func openJobEvents(cfg *config.Config, log *slog.Logger) (jobevent.Sink, func()) {
	dsn := cfg.JobEventsDSN
	if dsn == "" && cfg.JobEventsDSNFile != "" {
		raw, err := os.ReadFile(cfg.JobEventsDSNFile)
		if err != nil {
			log.Warn("job events are off: cannot read the database URL's file", "file", cfg.JobEventsDSNFile)
			return nil, func() {}
		}
		dsn = strings.TrimSpace(string(raw))
	}
	if dsn == "" {
		return nil, func() {}
	}
	store, err := jobstore.New(dsn, log)
	if err != nil {
		log.Warn("job events are off", "err", err)
		return nil, func() {}
	}
	return store, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		store.Close(ctx)
	}
}

// cacheCredentials is the refresher that keeps --cache-credentials-file
// holding the --cache-role-arn session's credentials.
func cacheCredentials(cfg *config.Config, api awscreds.STSAPI, log *slog.Logger) *awscreds.Refresher {
	return &awscreds.Refresher{
		API:     api,
		RoleARN: cfg.CacheRoleARN,
		Path:    cfg.CacheCredentialsFile,
		Log:     log.With("component", "cache-credentials"),
	}
}

// startCacheCredentials starts keeping --cache-credentials-file fresh when
// --cache-role-arn is set, with an STS client from newSTS. It returns the
// refresher, nil when the role is unset, and a stop that waits for it.
func startCacheCredentials(ctx context.Context, cfg *config.Config, newSTS func() (awscreds.STSAPI, error), log *slog.Logger) (*awscreds.Refresher, func(), error) {
	if cfg.CacheRoleARN == "" {
		return nil, func() {}, nil
	}
	api, err := newSTS()
	if err != nil {
		return nil, nil, fmt.Errorf("--cache-role-arn: %w", err)
	}
	r := cacheCredentials(cfg, api, log)
	return r, runInBackground(ctx, r.Run), nil
}

// runInBackground runs f until ctx ends or the returned stop is called;
// stop returns once f has.
func runInBackground(ctx context.Context, f func(context.Context)) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	events, closeEvents := openJobEvents(cfg, log)
	defer closeEvents()
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

	// One JWT source for the shard: every tenant is an installation of one App.
	appJWT, err := newAppJWT(ctx, cfg, privateKey)
	if err != nil {
		return err
	}

	// The local cache-server's S3 credentials, from a role that holds only
	// the bucket's policy, so it needs neither this host's role nor
	// instance metadata.
	cacheCreds, stopCreds, err := startCacheCredentials(ctx, cfg, func() (awscreds.STSAPI, error) {
		awsCfg, err := loadAWSConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return awssts.NewFromConfig(awsCfg), nil
	}, log)
	if err != nil {
		return err
	}
	defer stopCreds()

	// A static install is one tenant, made from its flags.
	t := tenant{
		Owner: owner, URL: cfg.URL, RunnerGroup: cfg.RunnerGroup, KeepOnClose: cfg.Owner != "",
		Token: cfg.Token,
		Tiers: cfg.EffectiveTiers(),
	}
	if appJWT != nil {
		t.Auth = ghauth.App{InstallationID: cfg.AppInstallID, JWT: appJWT}
	}
	s := &shard{cfg: cfg, log: log, events: events}

	// Each tier is its own GitHub scale set; developers pick one with
	// runs-on: <tier name>. One Firecracker provisioner backs every Firecracker
	// tier: they share the host kernel, tool cache, network and the MaxRunners
	// slot budget, while each keeps its own microVM shape and golden image.

	var hooks tenantHooks
	if firecrackerCache(cfg) || cfg.UsesEC2Cache() {
		jobs, err := jobidentity.NewRegistry(cfg.CacheJobsDir, log)
		if err != nil {
			return fmt.Errorf("cache job registry: %w", err)
		}
		defer jobs.Close()
		// Firecracker guests get identities only when their own cache is
		// on: one for the EC2 cache alone would give them scope there too.
		if firecrackerCache(cfg) {
			cfg.Firecracker.Jobs = jobs
		}
		s.cacheJobs = jobs
		if hooks, err = s.identityHooks(ctx, t); err != nil {
			return err
		}
	}
	if cfg.UsesEC2Cache() {
		ca, gitCA, err := loadCacheCAs(cfg)
		if err != nil {
			return err
		}
		s.ec2Cache = &ec2.CacheOptions{CA: ca, GitCA: gitCA, Upstream: cfg.EC2CacheAddr, Jobs: s.cacheJobs, LeafTTL: leafTTL(cfg)}
		log.Info("EC2 cache proxy enabled", "upstream", cfg.EC2CacheAddr, "leafTTL", s.ec2Cache.LeafTTL)
	}
	if cfg.UsesFirecracker() {
		s.fc = provisioner.NewFirecracker(cfg.Firecracker, log)
	}
	if cfg.UsesEC2() {
		jit, stopJIT, err := serveJIT(ctx, cfg, log)
		if err != nil {
			return err
		}
		// Instances still running their jobs fetch nothing more, but stop the
		// endpoint only after the drain all the same.
		defer stopJIT()
		s.jit = jit
		if s.ec2Client, err = newEC2Client(ctx, cfg, log); err != nil {
			return err
		}
	}

	served, err := s.start(ctx, t, hooks)
	if err != nil {
		return err
	}
	defer served.close(context.WithoutCancel(ctx))
	live := &liveTenants{}
	live.add(served)

	log.Info("firerunner starting",
		"url", cfg.URL,
		"tiers", len(served.tiers),
		"tenants", len(cfg.Tenants),
		"maxRunners", cfg.MaxRunners,
	)
	served.logTiers()

	if s.fc != nil {
		// Reclaim taps and job dirs orphaned by a previous unclean exit before
		// we start allocating new slots, so a crash-restart cycle can't exhaust
		// them.
		s.fc.CleanupStale(ctx)

		if err := s.fc.SetupNetwork(ctx); err != nil {
			return fmt.Errorf("setup network: %w", err)
		}
		go s.fc.RefreshNetwork(ctx)
	}

	// Publish each tier's listener health for status and doctor.
	healthPath := filepath.Join(cfg.Firecracker.WorkDir, health.FileName)
	stopHealth := publishHealth(healthPath, live.trackers, func(f *health.File) {
		f.Reaper = live.reaper()
		if s.ec2Client != nil {
			f.EC2Breakers = healthBreakers(s.ec2Client.Breakers())
		}
		f.CacheCredentials = healthCacheCredentials(cacheCreds)
	}, log)

	// Drive every tier's listener concurrently. A listener reconnects its own
	// session; one that stops on a fatal error leaves the other tiers running.
	// runCtx ends on SIGTERM, or once every tier has stopped. microVMs and the
	// warm-pool top-up bind to runCtx too, so either way listeners stop,
	// Reconcile no-ops, and Drain settles.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Each other tenant has a loop of its own, so that a slow start or a failure
	// there touches nobody else. They end with runCtx, and drain themselves.
	var tenants sync.WaitGroup
	for _, c := range cfg.Tenants {
		l := s.loop(extraTenant(cfg, c, owner, appJWT), live)
		tenants.Go(func() { l.run(runCtx) })
	}

	runErr := serveTiers(runCtx, cancel, served.names(), served.runs(cfg), log)
	cancel()
	// No tier is listening any more. Stop refreshing the health file before
	// the drain, which can wait out long jobs: a file that keeps its last
	// write time ages into "stale" instead of claiming live listeners.
	stopHealth()

	log.Info("shutdown signalled, draining in-flight microVMs")
	served.drain()
	tenants.Wait()
	log.Info("firerunner stopped")

	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}
