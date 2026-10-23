package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/health"
	"github.com/solcreek/firerunner/internal/listener"
	"github.com/solcreek/firerunner/internal/provisioner"
	"github.com/solcreek/firerunner/internal/scheduler"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"512B", 512, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"50GB", 50_000_000_000, false},
		{"2GiB", 2 << 30, false},
		{"1.5GB", 1_500_000_000, false},
		{" 10 mb ", 10_000_000, false},
		{"-5GB", 0, true},
		{"abc", 0, true},
		{"12xz", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCacheServerRejectsUnsafeScopeConfig(t *testing.T) {
	for _, args := range [][]string{
		{"--jobs-dir", "relative"},
		{"--repository", ""},
		{"--repository", "owner/repo", "--default-ref", "main"},
	} {
		args = append([]string{"--dir", t.TempDir()}, args...)
		if err := cacheServe(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCacheServerChecksIdentityDirectoryAtStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--jobs-dir", dir, "--jobs-owner-uid", strconv.Itoa(os.Getuid() + 1)},
		{"--jobs-dir", dir + "/missing"},
	} {
		args = append([]string{"--dir", t.TempDir()}, args...)
		if err := cacheServe(args); err == nil || !strings.Contains(err.Error(), "job identity directory") {
			t.Fatalf("startup check: %v", err)
		}
	}
}

// stubProvisioner stands in for the host's Firecracker provisioner.
type stubProvisioner struct{ name string }

func (s stubProvisioner) Launch(context.Context, string, string, core.RunnerSpec, func()) error {
	return nil
}
func (s stubProvisioner) Name() string { return s.name }

func TestTierProvisioner(t *testing.T) {
	fc := stubProvisioner{name: "firecracker"}
	ec2 := config.Tier{Name: "cloud", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}

	// Every Firecracker tier, implicit or explicit, shares the one host
	// provisioner and its slot budget.
	for _, tier := range []config.Tier{{Name: "a"}, {Name: "b", Provisioner: config.ProvisionerFirecracker}} {
		p, err := tierProvisioner(tier, fc, nil)
		if err != nil || p != fc {
			t.Fatalf("tier %q: got (%v, %v), want the shared firecracker provisioner", tier.Name, p, err)
		}
	}
	// A Firecracker tier on a host without the Firecracker provisioner is a
	// wiring bug and must fail before any scale set is registered.
	if _, err := tierProvisioner(config.Tier{Name: "a"}, nil, nil); err == nil {
		t.Fatal("expected an error for a firecracker tier without a firecracker provisioner")
	}
	// Each ec2 tier gets its own provisioner, built for that tier.
	var built []string
	newEC2 := func(t config.Tier) provisioner.Provisioner {
		built = append(built, t.Name)
		return stubProvisioner{name: "ec2:" + t.Name}
	}
	p, err := tierProvisioner(ec2, fc, newEC2)
	if err != nil || p.Name() != "ec2:cloud" || len(built) != 1 {
		t.Fatalf("ec2 tier: got (%v, %v), built %v", p, err, built)
	}
	if _, err := tierProvisioner(ec2, fc, nil); err == nil {
		t.Fatal("expected an error for an ec2 tier without the ec2 client")
	}
	if _, err := tierProvisioner(config.Tier{Name: "x", Provisioner: "lambda"}, fc, newEC2); err == nil {
		t.Fatal("expected an error for an unknown provisioner")
	}
}

func TestServeTiers_OneTierStoppingLeavesOthersRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bDone := make(chan struct{})
	runs := []func(context.Context) error{
		func(context.Context) error { return errors.New("401 Unauthorized") },
		func(ctx context.Context) error {
			defer close(bDone)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	res := make(chan error, 1)
	go func() { res <- serveTiers(ctx, cancel, []string{"a", "b"}, runs, slog.New(slog.DiscardHandler)) }()

	time.Sleep(50 * time.Millisecond) // tier a has failed by now
	select {
	case <-bDone:
		t.Fatal("tier b stopped because tier a failed")
	default:
	}
	// A shutdown signal still ends everything cleanly.
	cancel()
	if err := <-res; err != nil {
		t.Fatalf("serveTiers after shutdown = %v, want nil", err)
	}
}

func TestServeTiers_AllTiersStoppedShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := []func(context.Context) error{
		func(context.Context) error { return errors.New("a: 401") },
		func(context.Context) error { return errors.New("b: 403") },
	}
	err := serveTiers(ctx, cancel, []string{"a", "b"}, runs, slog.New(slog.DiscardHandler))
	// Which tier stopped first is up to the scheduler; either error names it.
	if err == nil || !(strings.Contains(err.Error(), `tier "a": a: 401`) || strings.Contains(err.Error(), `tier "b": b: 403`)) {
		t.Fatalf("err = %v, want one tier's error", err)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx not cancelled once every tier stopped")
	}
}

func TestPublishHealth(t *testing.T) {
	path := filepath.Join(t.TempDir(), health.FileName)
	a, b := health.NewTracker("a"), health.NewTracker("b")
	reaped := &health.Reaper{At: time.Unix(5, 0).UTC(), Instances: []string{"i-1"}}
	stop := publishHealth(path, []*health.Tracker{a, b}, func() *health.Reaper { return reaped }, slog.New(slog.DiscardHandler))
	f, err := health.Read(path)
	if err != nil || len(f.Tiers) != 2 || f.PID != os.Getpid() {
		t.Fatalf("initial file = %+v, %v", f, err)
	}
	b.Failed(errors.New("401 Unauthorized"))
	stop() // writes the final state
	f, err = health.Read(path)
	if err != nil || f.Tiers[1].State != health.Failed {
		t.Fatalf("final file = %+v, %v; want tier b failed", f, err)
	}
	if f.Reaper == nil || len(f.Reaper.Instances) != 1 {
		t.Fatalf("reaper state = %+v, want the latest sweep", f.Reaper)
	}
}

func TestServeTiers_StoppedTierContextIsCancelled(t *testing.T) {
	// A tier that stops gets its own context cancelled, which is what makes
	// its scheduler release idle runners, while the others' stay live.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctxs := make(chan context.Context, 2)
	runs := []func(context.Context) error{
		func(c context.Context) error { ctxs <- c; return errors.New("a: 401") },
		func(c context.Context) error { ctxs <- c; <-c.Done(); return c.Err() },
	}
	res := make(chan error, 1)
	go func() { res <- serveTiers(ctx, cancel, []string{"a", "b"}, runs, slog.New(slog.DiscardHandler)) }()
	got := []context.Context{<-ctxs, <-ctxs}
	var failed, live context.Context
	deadline := time.Now().Add(2 * time.Second)
	for failed == nil && time.Now().Before(deadline) {
		for _, c := range got {
			if c.Err() != nil {
				failed = c
			} else {
				live = c
			}
		}
		time.Sleep(time.Millisecond)
	}
	if failed == nil {
		t.Fatal("the stopped tier's context was never cancelled")
	}
	if live == nil || live.Err() != nil {
		t.Fatal("the serving tier's context was cancelled too")
	}
	cancel()
	if err := <-res; err != nil {
		t.Fatalf("serveTiers = %v", err)
	}
}

func TestServeTiers_RestartStopsEveryTier(t *testing.T) {
	// A deleted scale set needs a fresh process to register it again: every
	// tier stops and serveTiers returns the ErrRestart, so main exits non-zero
	// for systemd to restart it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := []func(context.Context) error{
		func(context.Context) error { return fmt.Errorf("%w: 404", listener.ErrRestart) },
		func(c context.Context) error { <-c.Done(); return c.Err() },
	}
	err := serveTiers(ctx, cancel, []string{"a", "b"}, runs, slog.New(slog.DiscardHandler))
	if !errors.Is(err, listener.ErrRestart) {
		t.Fatalf("serveTiers = %v, want ErrRestart", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the other tiers were left running")
	}
}

func TestTierPending(t *testing.T) {
	var shared scheduler.PendingLaunches
	if got := tierPending(config.Tier{Name: "fc"}, &shared); got != &shared {
		t.Fatal("a firecracker tier must share the host's pending counter")
	}
	if got := tierPending(config.Tier{Name: "cloud", Provisioner: config.ProvisionerEC2}, &shared); got != nil {
		t.Fatal("an ec2 tier must not share the firecracker tiers' pending counter")
	}
}

func TestReaperOptions(t *testing.T) {
	cfg := &config.Config{EC2Reaper: "dry-run"}
	cfg.Firecracker.MaxVMLifetime = 6 * time.Hour
	tiers := []config.Tier{
		{Name: "fc"},
		{Name: "cold", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}},
		{Name: "pooled", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{Pool: 2}},
	}
	// Only the Firecracker tier: building an ec2 tier's remover needs a live
	// listener. The fields and the unknown-tier path are what is checked.
	o := reaperOptions(cfg, "me", tiers[:1], []*listener.ScaleSet{nil}, slog.New(slog.DiscardHandler))
	if o.Owner != "me" || !o.DryRun || o.MaxLifetime != 6*time.Hour || len(o.PooledTiers) != 0 {
		t.Fatalf("options = %+v", o)
	}
	if err := o.Remove(context.Background(), "gone-tier", "r"); err == nil {
		t.Fatal("removing a runner of an unconfigured tier must fail, not succeed")
	}
}
