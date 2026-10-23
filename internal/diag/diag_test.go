package diag

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/cacheserver"
	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/health"
	"github.com/solcreek/firerunner/internal/provisioner"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0B",
		512:        "512B",
		1024:       "1.0KiB",
		1536:       "1.5KiB",
		1 << 20:    "1.0MiB",
		9993822208: "9.3GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestIsPID(t *testing.T) {
	for _, s := range []string{"1", "2474324"} {
		if !isPID(s) {
			t.Errorf("isPID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "cpuinfo", "12a", "self"} {
		if isPID(s) {
			t.Errorf("isPID(%q) = true, want false", s)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("Firecracker v1.10.1\nextra\n"); got != "Firecracker v1.10.1" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("  one line  "); got != "one line" {
		t.Errorf("firstLine (single) = %q", got)
	}
}

func TestEgressDesc(t *testing.T) {
	if got := egressDesc(nil); got != "none (deny-all)" {
		t.Errorf("egressDesc(nil) = %q, want deny-all (empty list drops all new egress)", got)
	}
	if got := egressDesc([]string{}); got != "none (deny-all)" {
		t.Errorf("egressDesc([]) = %q, want deny-all", got)
	}
	if got := egressDesc([]string{"open"}); got != "open" {
		t.Errorf("egressDesc(open) = %q", got)
	}
	if got := egressDesc([]string{"api", "git"}); got != "api+git" {
		t.Errorf("egressDesc(api,git) = %q", got)
	}
}

func TestProcMatchesInstance(t *testing.T) {
	direct := provisioner.FirecrackerConfig{WorkDir: "/var/tmp/firerunner"}
	jail := provisioner.FirecrackerConfig{WorkDir: "/var/tmp/firerunner", Jailer: true, ChrootBase: "/srv/jailer"}

	// Direct mode: this instance's VMM carries the work dir in its cmdline.
	if !procMatchesInstance("/usr/bin/firecracker --api-sock /var/tmp/firerunner/fr-abc/fc.sock --id fr-abc", "/", direct) {
		t.Error("own direct-mode VMM not matched")
	}
	// A peer instance with a different work dir must not match.
	if procMatchesInstance("/usr/bin/firecracker --api-sock /var/tmp/other/fk-xyz/fc.sock --id fk-xyz", "/", direct) {
		t.Error("peer direct-mode VMM wrongly matched")
	}
	// Jailer mode: the chrooted VMM's root resolves under ChrootBase.
	if !procMatchesInstance("/usr/bin/firecracker --api-sock /run/firecracker.socket --id fr-abc", "/srv/jailer/firecracker/fr-abc/root", jail) {
		t.Error("own jailer-mode VMM not matched")
	}
	if procMatchesInstance("/usr/bin/firecracker --api-sock /srv/other/firecracker/fk-xyz/root", "/srv/other/firecracker/fk-xyz/root", jail) {
		t.Error("peer jailer-mode VMM wrongly matched")
	}

	// Prefix-collision: a peer work dir that shares a string prefix with this
	// instance's (/var/tmp/firerunner vs /var/tmp/firerunner-kaikhq) must not be
	// counted. This is the exact case that inflated the live count.
	if procMatchesInstance("/usr/bin/firecracker --api-sock /var/tmp/firerunner-kaikhq/fk-xyz/fc.sock --id fk-xyz", "/", direct) {
		t.Error("peer work dir sharing a prefix wrongly matched")
	}
	peerBase := provisioner.FirecrackerConfig{Jailer: true, ChrootBase: "/srv/jailer"}
	if procMatchesInstance("/usr/bin/firecracker --api-sock /run/firecracker.socket", "/srv/jailer2/firecracker/fk-xyz/root", peerBase) {
		t.Error("peer chroot base sharing a prefix wrongly matched")
	}
}

func TestFileCheck(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "small.bin")
	if err := os.WriteFile(small, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if c := fileCheck("golden", big, 1<<20); c.Level != levelPass {
		t.Errorf("big file: level = %s, detail = %s", c.Level, c.Detail)
	}
	if c := fileCheck("golden", small, 1<<20); c.Level != levelWarn {
		t.Errorf("small file: level = %s, want WARN", c.Level)
	}
	if c := fileCheck("golden", filepath.Join(dir, "nope"), 1); c.Level != levelFail {
		t.Errorf("missing file: level = %s, want FAIL", c.Level)
	}
	if c := fileCheck("golden", "", 1); c.Level != levelFail {
		t.Errorf("empty path: level = %s, want FAIL", c.Level)
	}
}

func TestWorkdirCheck(t *testing.T) {
	dir := t.TempDir()
	if c := workdirCheck(""); c.Level != levelFail {
		t.Errorf("empty path: want FAIL, got %s", c.Level)
	}
	if c := workdirCheck(dir); c.Level == levelFail {
		t.Errorf("writable existing dir: unexpected FAIL: %s", c.Detail)
	}
	// A not-yet-existing dir under a writable parent is created on first run.
	if c := workdirCheck(filepath.Join(dir, "sub", "work")); c.Level != levelWarn {
		t.Errorf("missing dir, writable parent: want WARN, got %s (%s)", c.Level, c.Detail)
	}
	// A file where a dir is expected is a hard failure.
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := workdirCheck(f); c.Level != levelFail {
		t.Errorf("file-not-dir: want FAIL, got %s", c.Level)
	}
}

func TestLogDirCheck(t *testing.T) {
	dir := t.TempDir()
	if c := logDirCheck(dir); c.Level != levelPass {
		t.Errorf("writable dir: want PASS, got %s (%s)", c.Level, c.Detail)
	}
	if c := logDirCheck(filepath.Join(dir, "sub", "logs")); c.Level != levelWarn {
		t.Errorf("missing dir, writable parent: want WARN, got %s (%s)", c.Level, c.Detail)
	}
}

func TestReflinkLocalityCheck(t *testing.T) {
	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.ext4")
	if err := os.WriteFile(golden, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Golden and workdir under the same tempdir share a device.
	if c := reflinkLocalityCheck(dir, []string{golden}); c.Level != levelPass {
		t.Errorf("same-device golden: want PASS, got %s (%s)", c.Level, c.Detail)
	}
	// An empty golden list still passes (nothing to compare).
	if c := reflinkLocalityCheck(dir, nil); c.Level != levelPass {
		t.Errorf("no goldens: want PASS, got %s", c.Level)
	}
}

func TestParentWritable(t *testing.T) {
	dir := t.TempDir()
	if !parentWritable(filepath.Join(dir, "does-not-exist")) {
		t.Error("writable tempdir parent should report writable")
	}
	if !parentWritable(filepath.Join(dir, "a", "b", "c")) {
		t.Error("nearest existing ancestor (tempdir) is writable")
	}
}

func TestDeviceID(t *testing.T) {
	dir := t.TempDir()
	dev, ok := deviceID(dir)
	if !ok {
		t.Fatal("existing dir should have a device id")
	}
	// A missing sub-path resolves to the same device as its existing ancestor.
	if sub, ok := deviceID(filepath.Join(dir, "missing", "deep")); !ok || sub != dev {
		t.Errorf("missing sub-path device = %d ok=%v, want %d", sub, ok, dev)
	}
}

func TestAuthCheck(t *testing.T) {
	if c := authCheck(&config.Config{Token: "ghp_x"}); c.Level != levelPass {
		t.Errorf("token: %s %s", c.Level, c.Detail)
	}
	if c := authCheck(&config.Config{}); c.Level != levelFail {
		t.Errorf("no creds: want FAIL, got %s", c.Level)
	}
	if c := authCheck(&config.Config{AppClientID: "id"}); c.Level != levelFail {
		t.Errorf("app without installation id: want FAIL, got %s", c.Level)
	}
}

// TestDoctor_ReportsFailures checks that Doctor surfaces missing required inputs
// as FAILs, returns an error, and still prints every check line.
func TestDoctor_ReportsFailures(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir()}}
	var buf bytes.Buffer
	err := Doctor(cfg, "test", &buf, false)
	if err == nil {
		t.Fatal("Doctor returned nil error despite missing kernel/golden/ext-iface/auth")
	}
	out := buf.String()
	for _, want := range []string{"FAIL kernel", "FAIL golden", "FAIL ext-iface", "FAIL auth"} {
		if !strings.Contains(out, want) {
			t.Errorf("Doctor output missing %q\n%s", want, out)
		}
	}
}

// TestDoctor_ChecksPerTierGolden verifies that in tier-catalog mode Doctor
// checks each tier's golden (and toolcache) by name instead of the top-level
// --golden, which is legitimately empty when a tier catalog is configured.
func TestDoctor_ChecksPerTierGolden(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "golden.ext4")
	if err := os.WriteFile(big, make([]byte, 65<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Firecracker: provisioner.FirecrackerConfig{WorkDir: dir},
		Tiers: []config.Tier{
			{Name: "base", Golden: big},
			{Name: "missing", Golden: filepath.Join(dir, "nope.ext4")},
		},
	}
	r := runDoctor(cfg, "test")
	var names []string
	var basePass, missingFail bool
	for _, c := range r.Checks {
		names = append(names, c.Name)
		if c.Name == "golden[base]" && c.Level == levelPass {
			basePass = true
		}
		if c.Name == "golden[missing]" && c.Level == levelFail {
			missingFail = true
		}
		if c.Name == "golden" {
			t.Errorf("tier mode must not check the empty top-level golden; got %v", c)
		}
	}
	if !basePass {
		t.Errorf("expected golden[base] to pass; checks=%v", names)
	}
	if !missingFail {
		t.Errorf("expected golden[missing] to fail; checks=%v", names)
	}
}

// TestStatus_RendersPartialConfig ensures Status never errors on an unset/partial
// config and clearly marks unset images.
func TestStatus_RendersPartialConfig(t *testing.T) {
	cfg := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, false); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"firerunner", "golden", "(unset)", "microVMs: 0 active"} {
		if !strings.Contains(out, want) {
			t.Errorf("Status output missing %q\n%s", want, out)
		}
	}
}

// TestStatus_JSON ensures --json emits a well-formed StatusReport.
func TestStatus_JSON(t *testing.T) {
	cfg := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, true); err != nil {
		t.Fatalf("Status --json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal StatusReport: %v\n%s", err, buf.String())
	}
	if r.Version != "test" {
		t.Errorf("version = %q, want test", r.Version)
	}
	if r.ScaleSet != "firerunner" {
		t.Errorf("scale_set = %q, want firerunner", r.ScaleSet)
	}
}

// TestStatus_Cache ensures a configured dependency cache appears in both
// renderings, and that the default (off) is clearly marked.
func TestStatus_Cache(t *testing.T) {
	// Off by default.
	off := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var offBuf bytes.Buffer
	if err := Status(off, "test", &offBuf, false); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(offBuf.String(), "GitHub's hosted cache") {
		t.Errorf("default status should note GitHub's hosted cache\n%s", offBuf.String())
	}

	// Gateway mode (local port).
	gw := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16, CachePort: 8099}}
	var gwText, gwJSON bytes.Buffer
	if err := Status(gw, "test", &gwText, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	if !strings.Contains(gwText.String(), "gateway:8099") {
		t.Errorf("gateway status missing port\n%s", gwText.String())
	}
	if err := Status(gw, "test", &gwJSON, true); err != nil {
		t.Fatalf("Status json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(gwJSON.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Cache == nil || r.Cache.Mode != "gateway" || r.Cache.Port != 8099 {
		t.Errorf("cache json = %+v, want gateway/8099", r.Cache)
	}

	// URL mode.
	u := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16, CacheURL: "http://cache.internal:8099"}}
	var uText bytes.Buffer
	if err := Status(u, "test", &uText, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	if !strings.Contains(uText.String(), "http://cache.internal:8099") {
		t.Errorf("url status missing URL\n%s", uText.String())
	}
}

// TestStatus_CacheStats ensures live cache-server numbers, including the
// artifact-forwarding counters, are fetched from /stats and rendered: the
// artifact line appears exactly when there is something to report, and both
// forwarded RPCs and errors are shown.
func TestStatus_CacheStats(t *testing.T) {
	serve := func(st cacheserver.Stats) *config.Config {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/stats" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st)
		}))
		t.Cleanup(ts.Close)
		return &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{
			WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16, CacheURL: ts.URL,
		}}
	}
	render := func(cfg *config.Config) string {
		var buf bytes.Buffer
		if err := Status(cfg, "test", &buf, false); err != nil {
			t.Fatalf("Status: %v", err)
		}
		return buf.String()
	}

	// Cache numbers only: no artifact line when nothing has been forwarded.
	quiet := render(serve(cacheserver.Stats{Entries: 3, Bytes: 2048, Hits: 7, Misses: 1}))
	if !strings.Contains(quiet, "3 entries") || !strings.Contains(quiet, "7 hit / 1 miss") {
		t.Errorf("cache stats not rendered\n%s", quiet)
	}
	if strings.Contains(quiet, "artifact") {
		t.Errorf("artifact line shown with zero counters\n%s", quiet)
	}

	// Forwarded RPCs and errors are both reported.
	busy := render(serve(cacheserver.Stats{Entries: 3, ArtifactRPCs: 42, ArtifactErrors: 2}))
	if !strings.Contains(busy, "42 artifact rpcs forwarded upstream, 2 errors") {
		t.Errorf("artifact counters not rendered\n%s", busy)
	}

	// Errors alone (e.g. forwarding disabled) still surface.
	failing := render(serve(cacheserver.Stats{ArtifactErrors: 5}))
	if !strings.Contains(failing, "0 artifact rpcs forwarded upstream, 5 errors") {
		t.Errorf("error-only artifact counters not rendered\n%s", failing)
	}

	// The JSON report carries the raw counters for tooling.
	var out bytes.Buffer
	if err := Status(serve(cacheserver.Stats{ArtifactRPCs: 9, ArtifactErrors: 1}), "test", &out, true); err != nil {
		t.Fatalf("Status json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Cache == nil || r.Cache.Stats == nil || r.Cache.Stats.ArtifactRPCs != 9 || r.Cache.Stats.ArtifactErrors != 1 {
		t.Errorf("json stats = %+v, want artifact_rpcs=9 artifact_errors=1", r.Cache)
	}
}

// TestStatus_Tiers ensures a configured tier catalog appears in both renderings.
func TestStatus_Tiers(t *testing.T) {
	cfg := &config.Config{
		ScaleSetName: "firerunner",
		MaxRunners:   8,
		Firecracker:  provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16},
		Tiers: []config.Tier{
			{Name: "firerunner", VCPU: 2, MemMiB: 4096, Golden: "/g/base.ext4", Min: 1, Max: 8},
			{Name: "firerunner-8c16g", VCPU: 8, MemMiB: 16384, Golden: "/g/base.ext4", Min: 0, Max: 2},
		},
	}

	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	for _, want := range []string{"tiers: 2", "firerunner-8c16g", "16384MiB"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text status missing %q\n%s", want, text.String())
		}
	}

	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, true); err != nil {
		t.Fatalf("Status --json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Tiers) != 2 || r.Tiers[1].Name != "firerunner-8c16g" || r.Tiers[1].VCPU != 8 {
		t.Errorf("tiers = %+v", r.Tiers)
	}
}

// TestDoctor_JSON ensures --json emits a DoctorReport reflecting failures.
func TestDoctor_JSON(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir()}}
	var buf bytes.Buffer
	if err := Doctor(cfg, "test", &buf, true); err == nil {
		t.Fatal("Doctor --json returned nil error despite missing inputs")
	}
	var r DoctorReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal DoctorReport: %v\n%s", err, buf.String())
	}
	if r.OK {
		t.Error("report OK = true, want false")
	}
	if r.Failed == 0 || r.Failed != countFails(r.Checks) {
		t.Errorf("failed = %d, checks-derived = %d", r.Failed, countFails(r.Checks))
	}
}

func countFails(checks []Check) int {
	n := 0
	for _, c := range checks {
		if c.Level == levelFail {
			n++
		}
	}
	return n
}

// TestStatus_EC2Tier shows an ec2 tier by its instance types and market, with
// no microVM shape or golden image.
func TestStatus_EC2Tier(t *testing.T) {
	cfg := &config.Config{
		ScaleSetName: "firerunner",
		MaxRunners:   4,
		Firecracker:  provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16},
		Tiers: []config.Tier{
			{Name: "local", VCPU: 2, MemMiB: 4096, Golden: "/g/base.ext4", Max: 4},
			{Name: "cloud", Provisioner: config.ProvisionerEC2, Max: 20, EC2: &config.EC2Tier{
				InstanceTypes: []string{"c8a.xlarge", "c7a.xlarge"}, Market: config.MarketSpot,
			}},
		},
	}
	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	for _, want := range []string{"PROVISIONER", "firecracker", "c8a.xlarge,c7a.xlarge (spot)"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text status missing %q\n%s", want, text.String())
		}
	}

	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, true); err != nil {
		t.Fatalf("Status --json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	local, cloud := r.Tiers[0], r.Tiers[1]
	if local.Provisioner != "firecracker" || local.Golden == nil || local.VCPU != 2 {
		t.Errorf("local tier = %+v", local)
	}
	if cloud.Provisioner != "ec2" || cloud.Golden != nil || cloud.VCPU != 0 ||
		len(cloud.InstanceTypes) != 2 || cloud.Market != "spot" || cloud.Max != 20 {
		t.Errorf("cloud tier = %+v", cloud)
	}
}

// TestDoctor_SkipsEC2TierGolden: an ec2 tier has no golden on this host, so
// Doctor must not fail it on a missing image.
func TestDoctor_SkipsEC2TierGolden(t *testing.T) {
	cfg := &config.Config{
		Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir()},
		Tiers: []config.Tier{
			{Name: "cloud", Provisioner: config.ProvisionerEC2, Max: 4, EC2: &config.EC2Tier{}},
		},
	}
	for _, c := range runDoctor(cfg, "test").Checks {
		if strings.HasPrefix(c.Name, "golden") || strings.HasPrefix(c.Name, "toolcache") {
			t.Errorf("doctor checked an image for an ec2 tier: %v", c)
		}
	}
}

// writeListenerHealth publishes a daemon health file in dir, written at at.
func writeListenerHealth(t *testing.T, dir string, at time.Time, tiers ...health.Tier) {
	t.Helper()
	if err := health.Write(filepath.Join(dir, health.FileName), health.File{PID: 7, WrittenAt: at, Tiers: tiers}); err != nil {
		t.Fatal(err)
	}
}

func TestStatus_ShowsFailedTierListener(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeListenerHealth(t, dir, now,
		health.Tier{Name: "a", State: health.Failed, LastError: `status="401 Unauthorized"`, Since: now},
		health.Tier{Name: "b", State: health.Connected, Reconnects: 2, LastMessage: now, Since: now},
	)
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}

	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	for _, want := range []string{"listeners:", "failed", "401 Unauthorized", "connected"} {
		if !strings.Contains(out, want) {
			t.Errorf("status text missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "STALE") {
		t.Errorf("fresh health reported stale\n%s", out)
	}

	var js bytes.Buffer
	if err := Status(cfg, "test", &js, true); err != nil {
		t.Fatal(err)
	}
	var r StatusReport
	if err := json.Unmarshal(js.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Listeners == nil || len(r.Listeners.Tiers) != 2 || r.Listeners.Tiers[0].State != health.Failed || r.Listeners.Stale {
		t.Fatalf("listeners = %+v", r.Listeners)
	}
}

func TestStatus_StaleListenerHealth(t *testing.T) {
	dir := t.TempDir()
	writeListenerHealth(t, dir, time.Now().Add(-5*time.Minute), health.Tier{Name: "a", State: health.Connected})
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "STALE") {
		t.Fatalf("old health not marked stale\n%s", text.String())
	}
}

func TestListenerChecks(t *testing.T) {
	now := time.Now()
	level := func(checks []Check, name string) string {
		for _, c := range checks {
			if c.Name == name {
				return c.Level
			}
		}
		return "missing"
	}

	dir := t.TempDir()
	fc := provisioner.FirecrackerConfig{WorkDir: dir}
	// No daemon yet: nothing to judge, and doctor must not fail on it.
	if c := listenerChecks(fc, now); len(c) != 1 || c[0].Level != levelPass {
		t.Fatalf("no state file: %+v", c)
	}

	writeListenerHealth(t, dir, now,
		health.Tier{Name: "a", State: health.Failed, LastError: "401", Since: now},
		health.Tier{Name: "b", State: health.Reconnecting, LastError: "503", Since: now.Add(-time.Minute)},
		health.Tier{Name: "c", State: health.Connected},
	)
	c := listenerChecks(fc, now)
	if level(c, "listener[a]") != levelFail || level(c, "listener[b]") != levelWarn || level(c, "listener[c]") != levelPass {
		t.Fatalf("checks = %+v", c)
	}

	// A stale file only warns: its states may be out of date.
	writeListenerHealth(t, dir, now.Add(-time.Hour), health.Tier{Name: "a", State: health.Failed})
	if c := listenerChecks(fc, now); len(c) != 1 || c[0].Level != levelWarn {
		t.Fatalf("stale file: %+v", c)
	}
}

func TestDoctor_FailsOnFailedListener(t *testing.T) {
	dir := t.TempDir()
	writeListenerHealth(t, dir, time.Now(), health.Tier{Name: "a", State: health.Failed, LastError: "401"})
	r := runDoctor(&config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir}}, "test")
	found := false
	for _, c := range r.Checks {
		if c.Name == "listener[a]" && c.Level == levelFail {
			found = true
		}
	}
	if !found || r.OK {
		t.Fatalf("doctor did not fail on a failed tier listener: %+v", r.Checks)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 4); got != "abc" {
		t.Fatalf("truncate short = %q", got)
	}
}

func TestStatusAndDoctorShowDrift(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeListenerHealth(t, dir, now, health.Tier{
		Name: "a", State: health.Connected, Since: now,
		GitHub: &health.GitHubStats{Available: 2, Registered: 5, Tracked: 2, At: now},
		Drift:  "2 job(s) available but unacquired for 1m5s", Recycles: 1, LostRunners: 3,
	})
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"available 2", "registered 5", "firerunner tracks 2", "DRIFT: 2 job(s) available", "1 session recycle(s), 3 lost runner(s)"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("status missing %q\n%s", want, text.String())
		}
	}
	for _, c := range listenerChecks(cfg.Firecracker, now) {
		if c.Name == "listener[a]" && c.Level != levelWarn {
			t.Fatalf("doctor %s for a drifting tier, want WARN", c.Level)
		}
	}
}
