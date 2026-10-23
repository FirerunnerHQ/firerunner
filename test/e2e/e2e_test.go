//go:build e2e

// Package e2e boots real Firecracker microVMs. Run inside a disposable network
// namespace: the provisioner installs routes, taps and nftables rules there.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/cacheserver"
	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

func requireKVM(t *testing.T) (kernel, golden string) {
	t.Helper()
	missing := ""
	if _, err := os.Stat("/dev/kvm"); err != nil {
		missing = "/dev/kvm unavailable"
	}
	kernel, golden = os.Getenv("FR_KERNEL"), os.Getenv("FR_GOLDEN")
	if kernel == "" || golden == "" || os.Getenv("FR_EXT_IFACE") == "" {
		missing = "set FR_KERNEL, FR_GOLDEN and FR_EXT_IFACE"
	}
	if missing != "" {
		if os.Getenv("FR_REQUIRE_E2E") == "1" {
			t.Fatal(missing)
		}
		t.Skip(missing)
	}
	return kernel, golden
}

func config(t *testing.T, kernel, golden string) provisioner.FirecrackerConfig {
	t.Helper()
	return provisioner.FirecrackerConfig{
		KernelImage: kernel, GoldenRootFS: golden, WorkDir: t.TempDir(), LogDir: t.TempDir(),
		ExtIface: os.Getenv("FR_EXT_IFACE"), MaxVMs: 1, NetBase: 30, TapPrefix: "fre2e", NFTTable: "firerunner_e2e",
		// No external networking is needed. The separate netns suite checks the
		// allowlist; these tests exercise real guest -> host HTTP and source binding.
		Egress: provisioner.EgressConfig{Categories: []string{"open"}},
	}
}

func launch(t *testing.T, cfg provisioner.FirecrackerConfig, name, mode string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	f := provisioner.NewFirecracker(cfg, log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := f.Launch(ctx, name, "", core.RunnerSpec{VCPU: 1, MemMiB: 256, RootFS: cfg.GoldenRootFS}, nil)
	// Launch deliberately treats daemon cancellation as normal shutdown. Tests
	// must separately reject timeouts, or a guest that never boots falsely passes.
	console, readErr := os.ReadFile(filepath.Join(cfg.LogDir, name+".log"))
	if ctx.Err() != nil {
		t.Fatalf("guest timed out: %v\n%s", ctx.Err(), console)
	}
	if err != nil {
		t.Fatalf("Launch: %v\n%s", err, console)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(console), "firerunner-smoke: PASS "+mode) || strings.Contains(string(console), "firerunner-smoke: FAIL") {
		t.Fatalf("guest did not confirm success:\n%s", console)
	}
	if _, err := net.InterfaceByName("fre2e0"); err == nil {
		t.Fatal("guest tap survived teardown")
	}
	if f.FreeSlots() != 1 {
		t.Fatal("network slot was not released")
	}
	t.Logf("guest confirmed %s; VM exited and slot/tap reclaimed", mode)
}

func TestLaunchBootsAndSelfDestructs(t *testing.T) {
	kernel, golden := requireKVM(t)
	launch(t, config(t, kernel, golden), "e2e-boot", "boot")
}

type fixtureResolver struct{ identity jobidentity.Identity }

func (f fixtureResolver) Resolve(context.Context, jobidentity.Job) (jobidentity.Identity, error) {
	return f.identity, nil
}

// This fixture replaces only GitHub's control plane. Registry publication,
// per-VM IPs, firewall, cache HTTP, actual guest execution and teardown are real.
type fixtureJobs struct {
	registry *jobidentity.Registry
	reader   jobidentity.Reader
	identity *jobidentity.Identity
}

func (f fixtureJobs) Register(name, ip string) error {
	if err := f.registry.Register(name, ip); err != nil {
		return err
	}
	if f.identity == nil {
		return nil
	}
	f.registry.Start(context.Background(), jobidentity.Job{Runner: name, Repository: f.identity.Repository, Event: f.identity.Event, RunID: f.identity.RunID, JobID: f.identity.JobID}, fixtureResolver{*f.identity})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.reader.Lookup(ip); err == nil {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	_ = f.registry.Release(name)
	return fmt.Errorf("host fixture did not publish identity")
}
func (f fixtureJobs) Release(name string) error { return f.registry.Release(name) }

func TestCacheScopesAcrossMicroVMs(t *testing.T) {
	kernel, golden := requireKVM(t)
	dir := filepath.Join(t.TempDir(), "jobs")
	registry, err := jobidentity.NewRegistry(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	reader := jobidentity.Reader{Dir: dir}
	cache, err := cacheserver.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.SetScope(reader.Lookup); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: cache, ConnContext: cache.ConnectionContext, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	port := listener.Addr().(*net.TCPAddr).Port
	tests := []struct {
		mode, write string
		reads       []string
	}{
		{"main", "refs/heads/main", []string{"refs/heads/main"}},
		{"base", "refs/heads/release", []string{"refs/heads/release", "refs/heads/main"}},
		{"pr", "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/release", "refs/heads/main"}},
		{"sibling", "refs/pull/8/merge", []string{"refs/pull/8/merge", "refs/heads/release", "refs/heads/main"}},
		{"rerun", "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/release", "refs/heads/main"}},
		{"main-after", "refs/heads/main", []string{"refs/heads/main"}},
		{"unknown", "", nil},
		{"repo-b", "refs/heads/main", []string{"refs/heads/main"}},
		{"repo-a-again", "refs/heads/main", []string{"refs/heads/main"}},
		{"read-only", "", []string{"refs/heads/main"}},
	}
	for n, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := config(t, kernel, golden)
			jobs := fixtureJobs{registry: registry, reader: reader}
			if len(tt.reads) > 0 {
				jobs.identity = &jobidentity.Identity{Repository: "owner/repo", Event: "fixture", WriteRef: tt.write, ReadRefs: tt.reads, RunID: int64(n + 1), JobID: tt.mode}
			}
			if tt.mode == "repo-b" {
				jobs.identity.Repository = "owner/other"
			}
			cfg.Jobs = jobs
			cfg.CachePort = port
			cfg.BootArgs = provisioner.DefaultBootArgs + fmt.Sprintf(" fr_cache_mode=%s fr_cache_gateway=172.30.0.1 fr_cache_port=%d", tt.mode, port)
			launch(t, cfg, "e2e-"+tt.mode, tt.mode)
			if _, err := os.Stat(filepath.Join(dir, "172.30.0.2.json")); !os.IsNotExist(err) {
				t.Fatalf("job record survived VM exit: %v", err)
			}
		})
	}
}
