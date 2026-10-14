package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// shortSock returns a short-path unix socket (the sun_path limit is ~104 chars,
// and t.TempDir() paths are often too long on macOS).
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestBuildAPISteps_OrderAndPayloads(t *testing.T) {
	spec := core.RunnerSpec{VCPU: 4, MemMiB: 8192}
	steps := buildAPISteps("/vmlinux", "console=ttyS0 reboot=k ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off",
		"/job/rootfs.ext4", "", "fr0", "06:00:AC:10:00:02", "JIT-SECRET", nil, spec)

	wantOrder := []string{
		"/boot-source", "/drives/rootfs", "/machine-config",
		"/network-interfaces/eth0", "/mmds/config", "/mmds", "/actions",
	}
	if len(steps) != len(wantOrder) {
		t.Fatalf("got %d steps, want %d", len(steps), len(wantOrder))
	}
	idx := map[string]int{}
	for i, s := range steps {
		if s.path != wantOrder[i] {
			t.Fatalf("step %d = %q, want %q", i, s.path, wantOrder[i])
		}
		idx[s.path] = i
	}

	// Critical invariants.
	if idx["/mmds"] >= idx["/actions"] {
		t.Fatal("/mmds (JIT secret) must be set before InstanceStart")
	}
	if steps[len(steps)-1].path != "/actions" {
		t.Fatal("InstanceStart must be the last step")
	}

	mmds := steps[idx["/mmds"]].body.(map[string]any)
	if mmds["jitconfig"] != "JIT-SECRET" {
		t.Fatalf("mmds jitconfig = %v, want JIT-SECRET", mmds["jitconfig"])
	}
	mc := steps[idx["/machine-config"]].body.(map[string]any)
	if mc["vcpu_count"] != 4 || mc["mem_size_mib"] != 8192 {
		t.Fatalf("machine-config = %v, want vcpu 4 mem 8192", mc)
	}
	boot := steps[idx["/boot-source"]].body.(map[string]any)
	if boot["kernel_image_path"] != "/vmlinux" {
		t.Fatalf("boot kernel = %v", boot["kernel_image_path"])
	}
}

func TestConfigure_SendsSequenceOverUnixSocket(t *testing.T) {
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var paths []string
	bodies := map[string]map[string]any{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies[r.URL.Path] = m
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	f := NewFirecracker(FirecrackerConfig{KernelImage: "/k", GoldenRootFS: "/g"}, testLogger())
	err = f.configure(context.Background(), sock, "/k", "/rootfs", "", slotNet(0, "fr", 16), "console=ttyS0", "JIT", core.RunnerSpec{VCPU: 2, MemMiB: 512})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}

	want := []string{
		"/boot-source", "/drives/rootfs", "/machine-config",
		"/network-interfaces/eth0", "/mmds/config", "/mmds", "/actions",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != len(want) {
		t.Fatalf("got %d requests %v, want %d", len(paths), paths, len(want))
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("request %d = %q, want %q", i, paths[i], want[i])
		}
	}
	if bodies["/mmds"]["jitconfig"] != "JIT" {
		t.Fatalf("mmds body = %v", bodies["/mmds"])
	}
}

func TestConfigure_PropagatesAPIError(t *testing.T) {
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	f := NewFirecracker(FirecrackerConfig{KernelImage: "/k"}, testLogger())
	if err := f.configure(context.Background(), sock, "/k", "/rootfs", "", slotNet(0, "fr", 16), "console=ttyS0", "JIT", core.RunnerSpec{VCPU: 1, MemMiB: 128}); err == nil {
		t.Fatal("expected error from 400 response")
	}
}

func TestWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = os.WriteFile(sock, nil, 0o600)
	}()
	if err := waitForSocket(context.Background(), sock, time.Second); err != nil {
		t.Fatalf("waitForSocket: %v", err)
	}
}

func TestWaitForSocket_Timeout(t *testing.T) {
	if err := waitForSocket(context.Background(), "/nonexistent/x.sock", 80*time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForSocket_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForSocket(ctx, "/nonexistent/x.sock", time.Second); err == nil {
		t.Fatal("expected context error")
	}
}

func TestSetupNet_RunsExpectedCommands(t *testing.T) {
	var calls [][]string
	f := NewFirecracker(FirecrackerConfig{}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	if err := f.setupNet(context.Background(), slotNet(0, "fr", 16)); err != nil {
		t.Fatalf("setupNet: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("got %d commands, want 3: %v", len(calls), calls)
	}
	if calls[0][0] != "ip" || calls[0][1] != "tuntap" || calls[0][2] != "add" {
		t.Fatalf("first command = %v, want ip tuntap add", calls[0])
	}
	if calls[2][len(calls[2])-1] != "up" {
		t.Fatalf("last command = %v, want link set up", calls[2])
	}
}

func TestName(t *testing.T) {
	if got := NewFirecracker(FirecrackerConfig{}, testLogger()).Name(); got != "firecracker" {
		t.Fatalf("Name = %q", got)
	}
}

func TestTeardownNet(t *testing.T) {
	var got []string
	f := NewFirecracker(FirecrackerConfig{}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}
	if err := f.teardownNet(context.Background(), slotNet(0, "frtap", 16)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "ip" || got[1] != "link" || got[2] != "del" || got[3] != "frtap0" {
		t.Fatalf("teardown ran %v, want ip link del frtap0", got)
	}
}

func TestLogWriter(t *testing.T) {
	w := &logWriter{log: testLogger(), runner: "r", stream: "stdout"}
	n, err := w.Write([]byte("hello\n"))
	if err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
}

func TestCleanupStaleJobDirs(t *testing.T) {
	work := t.TempDir()
	// Two stale job dirs (each identified by an fc.sock) plus artefacts that must
	// survive: the nft rulesets and the logs dir.
	for _, name := range []string{"firerunner-aaaa1111", "firerunner-bbbb2222"} {
		dir := filepath.Join(work, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fc.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(work, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"firerunner.nft", "firerunner-forward.nft"} {
		if err := os.WriteFile(filepath.Join(work, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// MaxVMs=1 with an unusual prefix keeps the tap sweep from touching any real
	// host interface.
	f := NewFirecracker(FirecrackerConfig{WorkDir: work, TapPrefix: "frunittest", MaxVMs: 1}, testLogger())
	f.CleanupStale(context.Background())

	for _, name := range []string{"firerunner-aaaa1111", "firerunner-bbbb2222"} {
		if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
			t.Errorf("job dir %s not removed (err=%v)", name, err)
		}
	}
	for _, keep := range []string{"logs", "firerunner.nft", "firerunner-forward.nft"} {
		if _, err := os.Stat(filepath.Join(work, keep)); err != nil {
			t.Errorf("%s should be preserved: %v", keep, err)
		}
	}
}

func TestBuildJailerArgs(t *testing.T) {
	cfg := FirecrackerConfig{Binary: "/usr/local/bin/firecracker", ChrootBase: "/srv/jailer", JailUID: 955, JailGID: 954}
	got := buildJailerArgs(cfg, "firerunner-abc123", "")
	want := []string{
		"--id", "firerunner-abc123",
		"--exec-file", "/usr/local/bin/firecracker",
		"--uid", "955",
		"--gid", "954",
		"--cgroup-version", "2",
		"--chroot-base-dir", "/srv/jailer",
		"--new-pid-ns",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d args %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
	// No --netns when nsPath is empty: the jailed VMM stays in the host network
	// namespace so the per-slot host tap devices attach unchanged.
	for _, a := range got {
		if a == "--netns" {
			t.Fatal("buildJailerArgs must not pass --netns")
		}
	}
}

func TestBuildJailerArgs_NetNS(t *testing.T) {
	cfg := FirecrackerConfig{Binary: "/usr/local/bin/firecracker", ChrootBase: "/srv/jailer", JailUID: 955, JailGID: 954}
	got := buildJailerArgs(cfg, "firerunner-abc123", "/var/run/netns/frns0")
	found := false
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "--netns" && got[i+1] == "/var/run/netns/frns0" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildJailerArgs must pass --netns /var/run/netns/frns0, got %v", got)
	}
}

func TestBuildJailerArgs_CgroupLimits(t *testing.T) {
	cfg := FirecrackerConfig{
		Binary: "/usr/local/bin/firecracker", ChrootBase: "/srv/jailer", JailUID: 955, JailGID: 954,
		CgroupLimits: []string{"memory.max=2147483648", "cpu.max=200000", "pids.max=512"},
	}
	got := buildJailerArgs(cfg, "firerunner-abc123", "")
	// Each limit must appear as a separate --cgroup <file>=<value> pair.
	for _, want := range cfg.CgroupLimits {
		found := false
		for i := 0; i+1 < len(got); i++ {
			if got[i] == "--cgroup" && got[i+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing --cgroup %q in %v", want, got)
		}
	}
	// cgroup-version 2 must still be present.
	if !containsPair(got, "--cgroup-version", "2") {
		t.Errorf("expected --cgroup-version 2 in %v", got)
	}
}

func containsPair(args []string, k, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}

func TestJailCgroupDir(t *testing.T) {
	got := jailCgroupDir("/usr/local/bin/firecracker", "firerunner-abc123")
	want := "/sys/fs/cgroup/firecracker/firerunner-abc123"
	if got != want {
		t.Errorf("jailCgroupDir = %q, want %q", got, want)
	}
}

func TestBuildAPISteps_ToolCacheDrive(t *testing.T) {
	spec := core.RunnerSpec{VCPU: 2, MemMiB: 1024}
	// Without a tool cache the step list and order are unchanged.
	none := buildAPISteps("/vmlinux", "console=ttyS0", "/rootfs.ext4", "", "fr0", "06:00:AC:10:00:02", "JIT", nil, spec)
	for _, s := range none {
		if s.path == "/drives/toolcache" {
			t.Fatal("no toolcache drive expected when path is empty")
		}
	}

	// With one, a read-only non-root drive appears right after the rootfs, and
	// InstanceStart stays last.
	steps := buildAPISteps("/vmlinux", "console=ttyS0", "/rootfs.ext4", "/toolcache.ext4", "fr0", "06:00:AC:10:00:02", "JIT", nil, spec)
	if steps[len(steps)-1].path != "/actions" {
		t.Fatal("InstanceStart must remain last")
	}
	var rootfsIdx, tcIdx = -1, -1
	for i, s := range steps {
		switch s.path {
		case "/drives/rootfs":
			rootfsIdx = i
		case "/drives/toolcache":
			tcIdx = i
		}
	}
	if tcIdx == -1 {
		t.Fatal("expected a /drives/toolcache step")
	}
	if tcIdx != rootfsIdx+1 {
		t.Fatalf("toolcache drive at %d, want right after rootfs %d", tcIdx, rootfsIdx)
	}
	tc := steps[tcIdx].body.(map[string]any)
	if tc["path_on_host"] != "/toolcache.ext4" || tc["is_read_only"] != true || tc["is_root_device"] != false {
		t.Fatalf("toolcache drive body = %v, want ro non-root /toolcache.ext4", tc)
	}
}

func TestBuildAPISteps_CacheMMDS(t *testing.T) {
	spec := core.RunnerSpec{VCPU: 2, MemMiB: 1024}
	// No cache config -> the /mmds body carries only the JIT secret.
	none := buildAPISteps("/vmlinux", "console=ttyS0", "/rootfs.ext4", "", "fr0", "06:00:AC:10:00:02", "JIT", nil, spec)
	for _, s := range none {
		if s.path == "/mmds" {
			if _, ok := s.body.(map[string]any)["cache"]; ok {
				t.Fatal("no cache key expected when cache config is nil")
			}
		}
	}
	// With a cache config it is published under /mmds cache alongside jitconfig.
	steps := buildAPISteps("/vmlinux", "console=ttyS0", "/rootfs.ext4", "", "fr0", "06:00:AC:10:00:02", "JIT",
		map[string]any{"port": "8099"}, spec)
	for _, s := range steps {
		if s.path == "/mmds" {
			body := s.body.(map[string]any)
			if body["jitconfig"] != "JIT" {
				t.Fatalf("jitconfig lost: %v", body)
			}
			cache, ok := body["cache"].(map[string]any)
			if !ok || cache["port"] != "8099" {
				t.Fatalf("cache body = %v, want port 8099", body["cache"])
			}
		}
	}
}

func TestCacheMMDS(t *testing.T) {
	// Off by default.
	if got := NewFirecracker(FirecrackerConfig{}, testLogger()).cacheMMDS(); got != nil {
		t.Fatalf("unset = %v, want nil", got)
	}
	// Port -> guest builds the URL from its gateway.
	port := NewFirecracker(FirecrackerConfig{CachePort: 8099}, testLogger()).cacheMMDS()
	if port["port"] != "8099" {
		t.Fatalf("port = %v, want 8099", port)
	}
	// Explicit URL wins over port.
	both := NewFirecracker(FirecrackerConfig{CachePort: 8099, CacheURL: "http://cache:9000"}, testLogger()).cacheMMDS()
	if both["url"] != "http://cache:9000" {
		t.Fatalf("url = %v, want explicit URL to win", both)
	}
	if _, ok := both["port"]; ok {
		t.Fatalf("port should be omitted when url is set: %v", both)
	}
}

func TestFcToolCachePath(t *testing.T) {
	// Unset -> no drive.
	if got := NewFirecracker(FirecrackerConfig{}, testLogger()).fcToolCachePath(core.RunnerSpec{}); got != "" {
		t.Fatalf("unset = %q, want empty", got)
	}
	// Direct launch -> the host path is opened directly.
	direct := NewFirecracker(FirecrackerConfig{ToolCacheImage: "/var/lib/tc.ext4"}, testLogger())
	if got := direct.fcToolCachePath(core.RunnerSpec{}); got != "/var/lib/tc.ext4" {
		t.Fatalf("direct = %q, want host path", got)
	}
	// Jailer -> the in-jail staged path.
	jailed := NewFirecracker(FirecrackerConfig{ToolCacheImage: "/var/lib/tc.ext4", Jailer: true}, testLogger())
	if got := jailed.fcToolCachePath(core.RunnerSpec{}); got != "/toolcache.ext4" {
		t.Fatalf("jailer = %q, want /toolcache.ext4", got)
	}
	// A tier's own image overrides the global default (direct mode).
	if got := direct.fcToolCachePath(core.RunnerSpec{ToolCache: "/var/lib/tier.ext4"}); got != "/var/lib/tier.ext4" {
		t.Fatalf("tier override = %q, want tier host path", got)
	}
	// A per-tier image with no global default still resolves.
	none := NewFirecracker(FirecrackerConfig{}, testLogger())
	if got := none.fcToolCachePath(core.RunnerSpec{ToolCache: "/var/lib/tier.ext4"}); got != "/var/lib/tier.ext4" {
		t.Fatalf("tier-only = %q, want tier host path", got)
	}
}

func TestBuildAPISteps_InJailPaths(t *testing.T) {
	// Under the jailer, Firecracker sees the kernel and rootfs at chroot-relative
	// paths, not the host paths.
	steps := buildAPISteps("/vmlinux", "console=ttyS0", "/rootfs.ext4", "", "fr0", "06:00:AC:10:00:02", "JIT", nil, core.RunnerSpec{VCPU: 2, MemMiB: 1024})
	var boot, drive map[string]any
	for _, s := range steps {
		switch s.path {
		case "/boot-source":
			boot = s.body.(map[string]any)
		case "/drives/rootfs":
			drive = s.body.(map[string]any)
		}
	}
	if boot["kernel_image_path"] != "/vmlinux" {
		t.Fatalf("in-jail kernel = %v, want /vmlinux", boot["kernel_image_path"])
	}
	if drive["path_on_host"] != "/rootfs.ext4" {
		t.Fatalf("in-jail rootfs = %v, want /rootfs.ext4", drive["path_on_host"])
	}
}

func TestCleanupStale_JailDirs(t *testing.T) {
	base := t.TempDir()
	// Two stale jail roots left by a previous run.
	for _, id := range []string{"firerunner-jail1", "firerunner-jail2"} {
		root := jailChrootRoot(base, id)
		if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "rootfs.ext4"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := NewFirecracker(FirecrackerConfig{
		WorkDir: t.TempDir(), ChrootBase: base, Jailer: true, TapPrefix: "frunittest", MaxVMs: 1,
	}, testLogger())
	f.CleanupStale(context.Background())

	for _, id := range []string{"firerunner-jail1", "firerunner-jail2"} {
		if _, err := os.Stat(filepath.Join(base, "firecracker", id)); !os.IsNotExist(err) {
			t.Errorf("jail dir %s not removed (err=%v)", id, err)
		}
	}
}

func TestCleanupStale_CgroupDirs(t *testing.T) {
	// Redirect the cgroup mount to a temp dir so the sweep is testable off a real
	// cgroupfs.
	mount := t.TempDir()
	orig := cgroupV2Mount
	cgroupV2Mount = mount
	defer func() { cgroupV2Mount = orig }()

	parent := filepath.Join(mount, "firecracker")
	// A stale per-microVM cgroup dir (should be removed)...
	stale := filepath.Join(parent, "firerunner-node-abc")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	// ...and controller pseudo-files in the parent (must be left untouched).
	for _, name := range []string{"memory.max", "pids.max", "cgroup.procs"} {
		if err := os.WriteFile(filepath.Join(parent, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := NewFirecracker(FirecrackerConfig{
		Binary: "/usr/local/bin/firecracker", WorkDir: t.TempDir(), ChrootBase: t.TempDir(),
		Jailer: true, CgroupLimits: []string{"memory.max=1"}, TapPrefix: "frunittest", MaxVMs: 1,
	}, testLogger())
	f.CleanupStale(context.Background())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale cgroup dir not removed (err=%v)", err)
	}
	for _, name := range []string{"memory.max", "pids.max", "cgroup.procs"} {
		if _, err := os.Stat(filepath.Join(parent, name)); err != nil {
			t.Errorf("controller file %s wrongly removed: %v", name, err)
		}
	}
}

func TestSlotNetNS(t *testing.T) {
	s := slotNetNS(slotNet(3, "fn", 17), 17, "fn")
	if s.ns != "fnns3" {
		t.Errorf("ns = %q, want fnns3", s.ns)
	}
	if s.hostVeth != "fnv3h" || s.nsVeth != "fnv3g" {
		t.Errorf("veths = %q/%q, want fnv3h/fnv3g", s.hostVeth, s.nsVeth)
	}
	if len(s.hostVeth) > 15 || len(s.nsVeth) > 15 {
		t.Errorf("veth name exceeds IFNAMSIZ: %q/%q", s.hostVeth, s.nsVeth)
	}
	if s.hostVIP != "172.17.3.5" || s.nsVIP != "172.17.3.6" {
		t.Errorf("transit IPs = %q/%q, want .5/.6", s.hostVIP, s.nsVIP)
	}
	if s.nsPath != "/var/run/netns/fnns3" {
		t.Errorf("nsPath = %q", s.nsPath)
	}
}

func TestNetnsUpCommands(t *testing.T) {
	n := slotNet(0, "fn", 17)
	s := slotNetNS(n, 17, "fn")
	cmds := netnsUpCommands(n, s, 955, 954)
	if cmds[0][2] != "add" || cmds[0][3] != s.ns {
		t.Fatalf("first command must add the netns, got %v", cmds[0])
	}
	joined := make([]string, len(cmds))
	for i, c := range cmds {
		joined[i] = strings.Join(c, " ")
	}
	all := strings.Join(joined, "\n")
	wantContains := []string{
		"ip tuntap add dev " + n.tap + " mode tap user 955 group 954",
		"ip link add " + s.hostVeth + " type veth peer name " + s.nsVeth,
		"ip link set " + s.nsVeth + " netns " + s.ns,
		"ip netns exec " + s.ns + " ip route add default via " + s.hostVIP,
		"ip netns exec " + s.ns + " sysctl -q -w net.ipv4.ip_forward=1",
		"ip route add " + n.guestIP + "/32 via " + s.nsVIP,
	}
	for _, w := range wantContains {
		if !strings.Contains(all, w) {
			t.Errorf("netnsUpCommands missing %q\ngot:\n%s", w, all)
		}
	}
	// The tap must be created inside the namespace, not on the host.
	if !strings.Contains(all, "ip netns exec "+s.ns+" ip tuntap add dev "+n.tap) {
		t.Errorf("tap must be created inside the netns")
	}
}

func TestSetupNet_NetNSBranch(t *testing.T) {
	var calls [][]string
	f := NewFirecracker(FirecrackerConfig{NetNS: true, NetBase: 17, TapPrefix: "fn", JailUID: 955, JailGID: 954}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	if err := f.setupNet(context.Background(), slotNet(0, "fn", 17)); err != nil {
		t.Fatalf("setupNet: %v", err)
	}
	if len(calls) == 0 || calls[0][1] != "netns" || calls[0][2] != "add" {
		t.Fatalf("NetNS setup must start with ip netns add, got %v", calls)
	}
}

func TestTeardownNet_NetNSBranch(t *testing.T) {
	var calls [][]string
	f := NewFirecracker(FirecrackerConfig{NetNS: true, NetBase: 17, TapPrefix: "fn"}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	if err := f.teardownNet(context.Background(), slotNet(0, "fn", 17)); err != nil {
		t.Fatalf("teardownNet: %v", err)
	}
	if len(calls) == 0 || calls[0][1] != "netns" || calls[0][2] != "del" {
		t.Fatalf("NetNS teardown must start with ip netns del, got %v", calls)
	}
}

func TestCleanupStale_NetNS(t *testing.T) {
	// Redirect the named-netns dir so the stale sweep is testable off /var/run.
	dir := t.TempDir()
	orig := netnsRunDir
	netnsRunDir = dir
	defer func() { netnsRunDir = orig }()

	// A stale slot-0 netns marker; slot 1 has none.
	if err := os.WriteFile(filepath.Join(dir, "fnns0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	f := NewFirecracker(FirecrackerConfig{
		Binary: "/usr/local/bin/firecracker", WorkDir: t.TempDir(), ChrootBase: t.TempDir(),
		Jailer: true, NetNS: true, NetBase: 17, TapPrefix: "fn", MaxVMs: 2,
	}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		if name == "ip" && len(args) >= 3 && args[0] == "netns" && args[1] == "del" {
			deleted = append(deleted, args[2])
		}
		return nil
	}
	f.CleanupStale(context.Background())

	if len(deleted) != 1 || deleted[0] != "fnns0" {
		t.Fatalf("expected the stale netns fnns0 deleted exactly once, got %v", deleted)
	}
}

func TestPrepare_DirectLaunch(t *testing.T) {
	work := t.TempDir()
	var calls [][]string
	f := NewFirecracker(FirecrackerConfig{Binary: "/usr/bin/firecracker", KernelImage: "/host/vmlinux", WorkDir: work}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	cmd, sock, kernel, rootfs, cleanup, err := f.prepare(context.Background(), "job1", slotNet(0, "fr", 16), io.Discard, core.RunnerSpec{RootFS: "/golden.ext4"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer cleanup()
	if kernel != "/host/vmlinux" {
		t.Errorf("kernel = %q, want host path", kernel)
	}
	if rootfs != filepath.Join(work, "job1", "rootfs.ext4") {
		t.Errorf("rootfs = %q", rootfs)
	}
	if sock != filepath.Join(work, "job1", "fc.sock") {
		t.Errorf("sock = %q", sock)
	}
	if cmd.Path != "/usr/bin/firecracker" {
		t.Errorf("cmd = %q, want firecracker", cmd.Path)
	}
	if len(calls) != 1 || calls[0][0] != "cp" || calls[0][len(calls[0])-1] != rootfs {
		t.Errorf("expected one reflink cp into job dir, got %v", calls)
	}
}

func TestPrepare_Jailer(t *testing.T) {
	base := t.TempDir()
	var calls [][]string
	f := NewFirecracker(FirecrackerConfig{
		Binary: "/usr/local/bin/firecracker", JailerBin: "/usr/local/bin/jailer",
		KernelImage: "/host/vmlinux", ChrootBase: base, Jailer: true,
		JailUID: os.Getuid(), JailGID: os.Getgid(),
	}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		// Materialise the staged files so the chown in prepare succeeds.
		if name == "cp" {
			dst := args[len(args)-1]
			if err := os.WriteFile(dst, []byte("x"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	cmd, sock, kernel, rootfs, cleanup, err := f.prepare(context.Background(), "job2", slotNet(0, "fr", 16), io.Discard, core.RunnerSpec{RootFS: "/golden.ext4"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer cleanup()
	if kernel != "/vmlinux" || rootfs != "/rootfs.ext4" {
		t.Errorf("in-jail paths = %q,%q, want /vmlinux,/rootfs.ext4", kernel, rootfs)
	}
	wantSock := filepath.Join(jailChrootRoot(base, "job2"), "run", "firecracker.socket")
	if sock != wantSock {
		t.Errorf("sock = %q, want %q", sock, wantSock)
	}
	if cmd.Path != "/usr/local/bin/jailer" {
		t.Errorf("cmd = %q, want jailer", cmd.Path)
	}
	// Both kernel and rootfs must be staged into the chroot root.
	root := jailChrootRoot(base, "job2")
	for _, p := range []string{filepath.Join(root, "vmlinux"), filepath.Join(root, "rootfs.ext4")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected staged file %s: %v", p, err)
		}
	}
	if len(calls) != 2 {
		t.Errorf("expected 2 cp calls (kernel + rootfs), got %v", calls)
	}
}

func TestBusyDetectorFiresOnceOnMarker(t *testing.T) {
	var n int
	d := newBusyDetector(func() { n++ })
	if _, err := d.Write([]byte("√ Connected to GitHub\nListening for Jobs\n")); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fired %d times before marker, want 0", n)
	}
	if _, err := d.Write([]byte("2024-01-01 Running job: build\n")); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fired %d times, want 1", n)
	}
	// Further output must not fire again.
	if _, err := d.Write([]byte("Running job: another\nJob build completed\n")); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fired %d times after first, want 1", n)
	}
}

func TestBusyDetectorMatchesMarkerSplitAcrossWrites(t *testing.T) {
	var n int
	d := newBusyDetector(func() { n++ })
	for _, b := range []byte("noise Running job: build") {
		if _, err := d.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("fired %d times for byte-split marker, want 1", n)
	}
}

func TestBusyDetectorIdleStreamNeverFires(t *testing.T) {
	var n int
	d := newBusyDetector(func() { n++ })
	for i := 0; i < 100; i++ {
		if _, err := d.Write([]byte("Listening for Jobs\n")); err != nil {
			t.Fatal(err)
		}
	}
	if n != 0 {
		t.Fatalf("idle stream fired %d times, want 0", n)
	}
}

func TestBusyDetectorReportsFullWrite(t *testing.T) {
	d := newBusyDetector(func() {})
	p := []byte("some console output")
	n, err := d.Write(p)
	if err != nil || n != len(p) {
		t.Fatalf("Write = %d,%v want %d,nil", n, err, len(p))
	}
}

// TestConsoleBusyDetectionSurvivesLogWriteError models openConsole's writer
// stack: a failing log file (full/read-only disk) wrapped in tolerantWriter
// must not abort the io.MultiWriter, so the busy detector still fires. Without
// the tolerant wrapper the file error would stop the whole console pipe and the
// next SIGTERM would reap a busy VM as if it were idle.
func TestConsoleBusyDetectionSurvivesLogWriteError(t *testing.T) {
	var fired int
	detector := newBusyDetector(func() { fired++ })
	badFile := tolerantWriter{w: errWriter{}}
	mw := io.MultiWriter(detector, badFile)
	if _, err := mw.Write([]byte("Listening for Jobs\n")); err != nil {
		t.Fatalf("MultiWriter aborted on log error: %v", err)
	}
	if _, err := mw.Write([]byte("Running job: build\n")); err != nil {
		t.Fatalf("MultiWriter aborted on log error: %v", err)
	}
	if fired != 1 {
		t.Fatalf("busy detector fired %d times, want 1 (log error broke detection)", fired)
	}
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, io.ErrShortWrite }

func TestSanitizedEnv_ExcludesSecrets(t *testing.T) {
	t.Setenv("FR_TOKEN", "ghp_secret")
	t.Setenv("FR_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----")
	t.Setenv("HOME", "/home/firerunner")
	t.Setenv("PATH", "/custom/bin")

	env := sanitizedEnv()

	var hasPath, hasHome bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "FR_") {
			t.Errorf("sanitizedEnv leaked secret: %q", kv)
		}
		switch {
		case kv == "PATH=/custom/bin":
			hasPath = true
		case kv == "HOME=/home/firerunner":
			hasHome = true
		}
	}
	if !hasPath {
		t.Error("sanitizedEnv should forward PATH")
	}
	if !hasHome {
		t.Error("sanitizedEnv should forward HOME")
	}
}

func TestClassifyLaunchExit(t *testing.T) {
	const minRuntime = 3 * time.Second
	fastCrash := &exec.ExitError{ProcessState: &os.ProcessState{}}
	// nil wait error is always healthy.
	if err := classifyLaunchExit(nil, nil, 0, minRuntime); err != nil {
		t.Errorf("nil wait err should be healthy, got %v", err)
	}
	// We cancelled it (ctxErr set): any exit is expected, even a fast one.
	if err := classifyLaunchExit(fastCrash, context.Canceled, time.Millisecond, minRuntime); err != nil {
		t.Errorf("cancelled VM should be healthy, got %v", err)
	}
	// Non-zero exit after a healthy runtime is the reboot=k self-destruct.
	if err := classifyLaunchExit(fastCrash, nil, 10*time.Second, minRuntime); err != nil {
		t.Errorf("self-destruct after healthy runtime should be healthy, got %v", err)
	}
	// Non-zero exit within minRuntime is a boot failure.
	if err := classifyLaunchExit(fastCrash, nil, 200*time.Millisecond, minRuntime); err == nil {
		t.Error("fast non-zero exit should be a failure")
	}
	// A non-ExitError (I/O error, lost process) is always a failure.
	if err := classifyLaunchExit(io.ErrUnexpectedEOF, nil, time.Hour, minRuntime); err == nil {
		t.Error("non-ExitError should always be a failure")
	}
}

type fakeVMJobs struct {
	registered, released bool
	ip                   string
	failRelease          bool
}

func (j *fakeVMJobs) Register(name, ip string) error { j.registered = true; j.ip = ip; return nil }
func (j *fakeVMJobs) Release(name string) error {
	j.released = true
	if j.failRelease {
		return fmt.Errorf("disk error")
	}
	return nil
}

func TestLaunchRevokesIdentityBeforeSlotReuse(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			jobs := &fakeVMJobs{failRelease: fail}
			f := NewFirecracker(FirecrackerConfig{MaxVMs: 1, Jobs: jobs}, testLogger())
			// SetupNetwork fails before any host mutation (ExtIface is unset).
			if err := f.Launch(context.Background(), "runner", "jit", core.RunnerSpec{}, nil); err == nil {
				t.Fatal("expected failed launch")
			}
			if !jobs.registered || !jobs.released || jobs.ip != "172.16.0.2" {
				t.Fatalf("lifecycle %+v", jobs)
			}
			want := 1
			if fail {
				want = 0
			}
			if f.FreeSlots() != want {
				t.Fatalf("available slots %d want %d", f.FreeSlots(), want)
			}
		})
	}
}
