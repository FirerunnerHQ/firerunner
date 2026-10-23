package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseArgs is a minimal valid flag set; individual tests mutate a copy.
func baseArgs() []string {
	return []string{
		"--url", "https://github.com/org/repo",
		"--kernel", "/vmlinux",
		"--golden", "/golden.ext4",
		"--ext-iface", "eth0",
		"--token", "ghp_x",
	}
}

func TestFromFlags_Valid(t *testing.T) {
	c, err := FromFlags(append(baseArgs(),
		"--name", "fr", "--labels", "a, b ,,c", "--vcpu", "8", "--mem-mib", "16384", "--max-runners", "6"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.ScaleSetName != "fr" {
		t.Fatalf("name = %q", c.ScaleSetName)
	}
	if got := c.Labels; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("labels = %v, want [a b c]", got)
	}
	spec := c.RunnerSpec()
	if spec.VCPU != 8 || spec.MemMiB != 16384 || spec.RootFS != "/golden.ext4" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestFromFlags_CacheConfig(t *testing.T) {
	c, err := FromFlags(append(baseArgs(), "--cache-port", "8099"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.CachePort != 8099 {
		t.Fatalf("cache-port = %d, want 8099", c.Firecracker.CachePort)
	}

	c, err = FromFlags(append(baseArgs(), "--cache-url", "http://cache.internal:8099"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.CacheURL != "http://cache.internal:8099" {
		t.Fatalf("cache-url = %q", c.Firecracker.CacheURL)
	}

	// Off by default.
	c, err = FromFlags(baseArgs())
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.CachePort != 0 || c.Firecracker.CacheURL != "" {
		t.Fatalf("cache config should be off by default: port=%d url=%q", c.Firecracker.CachePort, c.Firecracker.CacheURL)
	}
}

func TestFromFlags_CacheConfigInvalid(t *testing.T) {
	cases := map[string][]string{
		"bad port": {"--cache-port", "70000"},
		"bad url":  {"--cache-url", "not-a-url"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(append(baseArgs(), extra...)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_MaxVMLifetime(t *testing.T) {
	// Defaults to a generous 6h backstop.
	c, err := FromFlags(baseArgs())
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.MaxVMLifetime != 6*time.Hour {
		t.Fatalf("default max-vm-lifetime = %s, want 6h", c.Firecracker.MaxVMLifetime)
	}

	// 0 disables it.
	c, err = FromFlags(append(baseArgs(), "--max-vm-lifetime", "0"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.MaxVMLifetime != 0 {
		t.Fatalf("max-vm-lifetime = %s, want 0 (disabled)", c.Firecracker.MaxVMLifetime)
	}

	// A too-small non-zero value is rejected so a real job is never truncated.
	if _, err := FromFlags(append(baseArgs(), "--max-vm-lifetime", "5s")); err == nil {
		t.Fatal("expected error for --max-vm-lifetime below 1m")
	}
}

func TestFromFlags_MissingRequired(t *testing.T) {
	cases := map[string][]string{
		"no url":    {"--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0", "--token", "t"},
		"no kernel": {"--url", "u", "--golden", "/g", "--ext-iface", "eth0", "--token", "t"},
		"no golden": {"--url", "u", "--kernel", "/k", "--ext-iface", "eth0", "--token", "t"},
		"no iface":  {"--url", "u", "--kernel", "/k", "--golden", "/g", "--token", "t"},
		"no auth":   {"--url", "u", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(args); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_BoundsValidation(t *testing.T) {
	cases := map[string][]string{
		"vcpu":  {"--vcpu", "0"},
		"mem":   {"--mem-mib", "64"},
		"maxrn": {"--max-runners", "0"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(append(baseArgs(), extra...)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_EnvFallback(t *testing.T) {
	t.Setenv("FR_URL", "https://github.com/org/repo")
	t.Setenv("FR_KERNEL", "/env-kernel")
	t.Setenv("FR_GOLDEN", "/env-golden")
	t.Setenv("FR_EXT_IFACE", "eth0")
	t.Setenv("FR_TOKEN", "ghp_env")
	t.Setenv("FR_VCPU", "2")

	c, err := FromFlags(nil)
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.KernelImage != "/env-kernel" || c.VCPU != 2 || c.Token != "ghp_env" {
		t.Fatalf("env fallback not applied: %+v", c)
	}
}

// A malformed typed env var must fail closed rather than silently reverting to
// the default (the pre-fix fail-open behaviour).
func TestParse_MalformedEnvFailsClosed(t *testing.T) {
	cases := map[string]string{
		"FR_VCPU":         "notanint",
		"FR_JAILER":       "yesplease",
		"FR_META_REFRESH": "5parsecs",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, val)
			_, err := Parse(nil)
			if err == nil {
				t.Fatalf("%s=%q should error, got nil", key, val)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error should name %s, got: %v", key, err)
			}
		})
	}
}

func TestFromFlags_AppCredsSatisfyAuth(t *testing.T) {
	args := []string{
		"--url", "u", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0",
		"--app-client-id", "Iv1.abc",
	}
	if _, err := FromFlags(args); err != nil {
		t.Fatalf("app creds should satisfy auth: %v", err)
	}
}

func TestFromFlags_JailerCgroup(t *testing.T) {
	c, err := FromFlags(append(baseArgs(),
		"--jailer", "--jail-uid", "955", "--jail-gid", "954",
		"--jailer-cgroup", "memory.max=2147483648;cpu.max=200000 ; ;pids.max=512"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	got := c.Firecracker.CgroupLimits
	want := []string{"memory.max=2147483648", "cpu.max=200000", "pids.max=512"}
	if len(got) != len(want) {
		t.Fatalf("cgroup limits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("limit %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFromFlags_JailerCgroupErrors(t *testing.T) {
	cases := map[string][]string{
		"without jailer": {"--jailer-cgroup", "memory.max=1"},
		"bad format": {
			"--jailer", "--jail-uid", "955", "--jail-gid", "954",
			"--jailer-cgroup", "memory.max=1;bogus",
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(append(baseArgs(), extra...)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_NetNS(t *testing.T) {
	c, err := FromFlags(append(baseArgs(),
		"--jailer", "--jail-uid", "955", "--jail-gid", "954", "--netns"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if !c.Firecracker.NetNS {
		t.Fatal("NetNS should be true")
	}
}

func TestFromFlags_NetNSRequiresJailer(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--netns")); err == nil {
		t.Fatal("expected error: --netns requires --jailer")
	}
}

func TestFromFlags_NetNSCachePortRejected(t *testing.T) {
	jail := []string{"--jailer", "--jail-uid", "955", "--jail-gid", "954", "--netns"}
	if _, err := FromFlags(append(append(baseArgs(), jail...), "--cache-port", "8099")); err == nil {
		t.Fatal("expected error: --cache-port is unreachable from inside --netns")
	}
	// --cache-url stays valid with --netns (routable address, not the in-ns gateway).
	if _, err := FromFlags(append(append(baseArgs(), jail...), "--cache-url", "http://cache.internal:8099")); err != nil {
		t.Fatalf("--netns with --cache-url should be valid: %v", err)
	}
}

func TestFromFlags_TapPrefix(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--tap-prefix", "frk")); err != nil {
		t.Fatalf("letters-only tap-prefix should be valid: %v", err)
	}
	bad := map[string][]string{
		"digit":      {"--tap-prefix", "fr2"},
		"symbol":     {"--tap-prefix", "fr-"},
		"too long":   {"--tap-prefix", "abcdefghijkl"},
		"underscore": {"--tap-prefix", "fr_"},
	}
	for name, extra := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(append(baseArgs(), extra...)); err == nil {
				t.Fatalf("expected error for %q tap-prefix", name)
			}
		})
	}
}

func TestFromFlags_ToolCache(t *testing.T) {
	img := filepath.Join(t.TempDir(), "toolcache-go.ext4")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := FromFlags(append(baseArgs(), "--toolcache", img))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.ToolCacheImage != img {
		t.Fatalf("ToolCacheImage = %q, want %q", c.Firecracker.ToolCacheImage, img)
	}
}

func TestFromFlags_ToolCacheMissing(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--toolcache", "/no/such/toolcache.ext4")); err == nil {
		t.Fatal("expected error for a missing --toolcache image")
	}
}

func TestEffectiveTiers_DefaultSingleTier(t *testing.T) {
	c, err := FromFlags(append(baseArgs(), "--name", "fr", "--vcpu", "2", "--mem-mib", "4096", "--max-runners", "8", "--min-runners", "1"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	tiers := c.EffectiveTiers()
	if len(tiers) != 1 {
		t.Fatalf("want 1 synthesized tier, got %d", len(tiers))
	}
	tr := tiers[0]
	if tr.Name != "fr" || tr.VCPU != 2 || tr.MemMiB != 4096 || tr.Golden != "/golden.ext4" || tr.Min != 1 || tr.Max != 8 {
		t.Fatalf("synthesized tier = %+v", tr)
	}
	if spec := tr.Spec(); spec.VCPU != 2 || spec.RootFS != "/golden.ext4" {
		t.Fatalf("tier spec = %+v", spec)
	}
}

func writeTiers(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tiers.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromFlags_TierCatalog(t *testing.T) {
	p := writeTiers(t, `[
	  {"name":"firerunner","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":1,"max":8},
	  {"name":"firerunner-8c16g","vcpu":8,"mem_mib":16384,"golden":"/g/base.ext4","min":0,"max":2},
	  {"name":"firerunner-node","labels":["node"],"vcpu":2,"mem_mib":4096,"golden":"/g/node.ext4","min":0,"max":4}
	]`)
	c, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	tiers := c.EffectiveTiers()
	if len(tiers) != 3 {
		t.Fatalf("want 3 tiers, got %d", len(tiers))
	}
	if tiers[1].Name != "firerunner-8c16g" || tiers[1].VCPU != 8 || tiers[1].MemMiB != 16384 {
		t.Fatalf("tier[1] = %+v", tiers[1])
	}
	if tiers[2].Golden != "/g/node.ext4" || len(tiers[2].Labels) != 1 || tiers[2].Labels[0] != "node" {
		t.Fatalf("tier[2] = %+v", tiers[2])
	}
}

func TestFromFlags_TierToolCache(t *testing.T) {
	// A per-tier toolcache is parsed, validated (must exist) and flows into the
	// tier's RunnerSpec, overriding the global default.
	img := filepath.Join(t.TempDir(), "tier-tc.ext4")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := writeTiers(t, `[
	  {"name":"lean","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","toolcache":"`+img+`","min":0,"max":4},
	  {"name":"fat","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":0,"max":4}
	]`)
	c, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	tiers := c.EffectiveTiers()
	if tiers[0].ToolCache != img || tiers[0].Spec().ToolCache != img {
		t.Fatalf("lean tier toolcache = %q / spec %q, want %q", tiers[0].ToolCache, tiers[0].Spec().ToolCache, img)
	}
	if tiers[1].ToolCache != "" || tiers[1].Spec().ToolCache != "" {
		t.Fatalf("fat tier toolcache = %q, want empty (global fallback)", tiers[1].ToolCache)
	}
}

func TestFromFlags_TierMaxExceedsBudget(t *testing.T) {
	// A tier whose max is larger than --max-runners can never reach it, so the
	// catalog is rejected rather than silently clamped.
	p := writeTiers(t, `[{"name":"big","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":0,"max":8}]`)
	if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "4")); err == nil {
		t.Fatal("expected error when a tier max exceeds --max-runners")
	}
	// Equal to the budget is fine.
	if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8")); err != nil {
		t.Fatalf("tier max == budget should be valid: %v", err)
	}
}

func TestFromFlags_TierToolCacheMissing(t *testing.T) {
	p := writeTiers(t, `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","toolcache":"/no/such/tc.ext4","min":0,"max":4}]`)
	if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8")); err == nil {
		t.Fatal("expected error for a missing per-tier toolcache image")
	}
}

func TestFromFlags_TierCatalogSatisfiesGolden(t *testing.T) {
	// A tier catalog replaces the top-level --golden requirement.
	p := writeTiers(t, `[{"name":"firerunner","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":0,"max":4}]`)
	args := []string{"--url", "u", "--kernel", "/k", "--ext-iface", "eth0", "--token", "t", "--tiers", p}
	if _, err := FromFlags(args); err != nil {
		t.Fatalf("tier catalog should satisfy the golden requirement: %v", err)
	}
}

func TestFromFlags_TierCatalogErrors(t *testing.T) {
	cases := map[string]string{
		"empty array":     `[]`,
		"missing name":    `[{"vcpu":2,"mem_mib":4096,"golden":"/g","max":2}]`,
		"low vcpu":        `[{"name":"t","vcpu":0,"mem_mib":4096,"golden":"/g","max":2}]`,
		"low mem":         `[{"name":"t","vcpu":2,"mem_mib":64,"golden":"/g","max":2}]`,
		"no golden":       `[{"name":"t","vcpu":2,"mem_mib":4096,"max":2}]`,
		"zero max":        `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":0}]`,
		"min over max":    `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","min":3,"max":2}]`,
		"duplicate names": `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":2},{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":2}]`,
		"bad json":        `{not json}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTiers(t, body)
			if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8")); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_TierWarmPoolsExceedBudget(t *testing.T) {
	// Warm pools sum to 5 but the shared slot budget is 4.
	p := writeTiers(t, `[
	  {"name":"a","vcpu":2,"mem_mib":4096,"golden":"/g","min":3,"max":4},
	  {"name":"b","vcpu":2,"mem_mib":4096,"golden":"/g","min":2,"max":4}
	]`)
	if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "4")); err == nil {
		t.Fatal("expected error: warm pools exceed the shared slot budget")
	}
}

func TestFromFlags_TierFileMissing(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--tiers", "/no/such/tiers.json")); err == nil {
		t.Fatal("expected error for a missing tiers file")
	}
}

func TestParseLevel(t *testing.T) {
	if parseLevel("debug").String() != "DEBUG" {
		t.Fatal("debug")
	}
	if parseLevel("warn").String() != "WARN" {
		t.Fatal("warn")
	}
	if parseLevel("bogus").String() != "INFO" {
		t.Fatal("default should be INFO")
	}
}

func TestResolvePrivateKey(t *testing.T) {
	// Inline PEM contents are returned as-is.
	pem := "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"
	c := &Config{AppPrivateKey: pem}
	if got, err := c.ResolvePrivateKey(); err != nil || got != pem {
		t.Fatalf("inline PEM: got %q err %v", got, err)
	}

	// Empty stays empty.
	if got, err := (&Config{}).ResolvePrivateKey(); err != nil || got != "" {
		t.Fatalf("empty: got %q err %v", got, err)
	}

	// A path is read from disk.
	dir := t.TempDir()
	path := dir + "/key.pem"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		t.Fatal(err)
	}
	c = &Config{AppPrivateKey: path}
	if got, err := c.ResolvePrivateKey(); err != nil || got != pem {
		t.Fatalf("path: got %q err %v", got, err)
	}

	// A missing path errors.
	if _, err := (&Config{AppPrivateKey: dir + "/missing.pem"}).ResolvePrivateKey(); err == nil {
		t.Fatal("expected error for missing key file")
	}
}

// TestResolvePrivateKeyNeverLeaksValue verifies that key material which fails
// the PEM-header check (so it is treated as a path) never appears in the error,
// which would otherwise flow into `doctor --json` and journald.
func TestResolvePrivateKeyNeverLeaksValue(t *testing.T) {
	// Multi-line key material without the exact "PRIVATE KEY" header.
	secret := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSj\nAgEAAoIBAQDsecretmaterial=="
	_, err := (&Config{AppPrivateKey: secret}).ResolvePrivateKey()
	if err == nil {
		t.Fatal("expected error for non-PEM, non-path value")
	}
	if strings.Contains(err.Error(), "secretmaterial") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked key material: %v", err)
	}

	// Single-line base64-looking material treated as a (missing) path must not
	// echo the value either.
	oneLine := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwsecretmaterialAAAA"
	_, err = (&Config{AppPrivateKey: oneLine}).ResolvePrivateKey()
	if err == nil {
		t.Fatal("expected error for missing key path")
	}
	if strings.Contains(err.Error(), "secretmaterial") || strings.Contains(err.Error(), oneLine) {
		t.Fatalf("error leaked key material: %v", err)
	}
}

func TestFromFlags_VMConnectTimeout(t *testing.T) {
	// Defaults to 5m: generous for a slow boot + registration, short enough
	// that a wedged VM frees its slot well before the 6h lifetime backstop.
	c, err := FromFlags(baseArgs())
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.ConnectTimeout != 5*time.Minute {
		t.Fatalf("default vm-connect-timeout = %s, want 5m", c.Firecracker.ConnectTimeout)
	}

	// 0 disables it.
	c, err = FromFlags(append(baseArgs(), "--vm-connect-timeout", "0"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.ConnectTimeout != 0 {
		t.Fatalf("vm-connect-timeout = %s, want 0 (disabled)", c.Firecracker.ConnectTimeout)
	}

	// Env var is honoured.
	t.Setenv("FR_VM_CONNECT_TIMEOUT", "90s")
	c, err = FromFlags(baseArgs())
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.ConnectTimeout != 90*time.Second {
		t.Fatalf("vm-connect-timeout from env = %s, want 90s", c.Firecracker.ConnectTimeout)
	}

	// Too small is rejected so a slow but healthy boot is never killed.
	if _, err := FromFlags(append(baseArgs(), "--vm-connect-timeout", "5s")); err == nil {
		t.Fatal("expected error for --vm-connect-timeout below 30s")
	}

	// It must be shorter than the lifetime backstop, or it could never fire first.
	if _, err := FromFlags(append(baseArgs(), "--vm-connect-timeout", "2h", "--max-vm-lifetime", "1h")); err == nil {
		t.Fatal("expected error for --vm-connect-timeout >= --max-vm-lifetime")
	}
	// ...unless the lifetime backstop is disabled.
	if _, err := FromFlags(append(baseArgs(), "--vm-connect-timeout", "2h", "--max-vm-lifetime", "0")); err != nil {
		t.Fatalf("connect timeout with lifetime disabled: %v", err)
	}
}

// ec2TierJSON is a valid EC2 tier; tests splice it into catalogs. The IDs are
// placeholders in AWS's format, not real resources.
// jitAddr is the --jit-addr every catalog with an ec2 tier needs.
var jitAddr = []string{"--jit-addr", "10.0.0.5:8098"}

const ec2TierJSON = `{"name":"cloud-4c","provisioner":"ec2","labels":["cloud"],"min":1,"max":20,
  "ec2":{"instance_types":["c8a.xlarge","c7a.xlarge"],"ami":"/firerunner/ami/runner",
         "subnets":["subnet-aaaa","subnet-bbbb"],"security_groups":["sg-cccc"],
         "market":"spot-then-on-demand","root_volume_gib":40}}`

func TestFromFlags_MixedProvisionerCatalog(t *testing.T) {
	// One process serves a Firecracker tier and an EC2 tier side by side.
	p := writeTiers(t, `[
	  {"name":"local","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":1,"max":4},
	  `+ec2TierJSON+`
	]`)
	c, err := FromFlags(append(append(baseArgs(), jitAddr...), "--tiers", p, "--max-runners", "4"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	tiers := c.EffectiveTiers()
	if tiers[0].IsEC2() || tiers[0].ProvisionerName() != ProvisionerFirecracker {
		t.Fatalf("tier[0] provisioner = %q, want firecracker by default", tiers[0].ProvisionerName())
	}
	cloud := tiers[1]
	if !cloud.IsEC2() || cloud.EC2 == nil {
		t.Fatalf("tier[1] = %+v, want an ec2 tier", cloud)
	}
	e := cloud.EC2
	if len(e.InstanceTypes) != 2 || e.InstanceTypes[0] != "c8a.xlarge" || e.InstanceTypes[1] != "c7a.xlarge" {
		t.Fatalf("instance_types = %v, want order preserved", e.InstanceTypes)
	}
	if len(e.Subnets) != 2 || len(e.SecurityGroups) != 1 || e.Market != MarketSpotThenOnDemand || e.RootVolumeGiB != 40 || e.Pool != 0 {
		t.Fatalf("ec2 block = %+v", e)
	}
	if !e.AMIFromSSM() || e.VolumeType() != "gp3" {
		t.Fatalf("AMIFromSSM=%v VolumeType=%q, want SSM name and gp3 default", e.AMIFromSSM(), e.VolumeType())
	}
	// The EC2 tier's max (20) is far above --max-runners (4): EC2 tiers draw no
	// host slots, so only Firecracker tiers are bounded by the budget.
	if cloud.Max != 20 {
		t.Fatalf("ec2 max = %d", cloud.Max)
	}
	if !c.UsesFirecracker() {
		t.Fatal("UsesFirecracker = false with a firecracker tier present")
	}
}

func TestEC2Tier_AMIKinds(t *testing.T) {
	cases := map[string]bool{
		"ami-0123456789abcdef0":           false,
		"/firerunner/ami/runner":          true,
		"firerunner-runner-ami":           true,
		"resolve:ssm:/aws/service/x/y/id": true,
	}
	for ami, ssm := range cases {
		if got := (EC2Tier{AMI: ami}).AMIFromSSM(); got != ssm {
			t.Errorf("AMIFromSSM(%q) = %v, want %v", ami, got, ssm)
		}
	}
	if got := (EC2Tier{RootVolumeType: "gp2"}).VolumeType(); got != "gp2" {
		t.Errorf("VolumeType = %q, want gp2", got)
	}
}

func TestFromFlags_EC2OnlyCatalogNeedsNoFirecrackerHost(t *testing.T) {
	// A control VM serving only EC2 tiers has no KVM, kernel or microVM
	// egress interface, so those flags are not required.
	p := writeTiers(t, `[`+ec2TierJSON+`]`)
	c, err := FromFlags(append([]string{"--url", "https://github.com/org", "--token", "t", "--tiers", p}, jitAddr...))
	if err != nil {
		t.Fatalf("EC2-only catalog without --kernel/--ext-iface: %v", err)
	}
	if c.UsesFirecracker() {
		t.Fatal("UsesFirecracker = true for an EC2-only catalog")
	}
	// The same flags are still required as soon as a Firecracker tier joins.
	mixed := writeTiers(t, `[{"name":"local","vcpu":2,"mem_mib":4096,"golden":"/g","max":2},`+ec2TierJSON+`]`)
	if _, err := FromFlags(append([]string{"--url", "u", "--token", "t", "--tiers", mixed}, jitAddr...)); err == nil {
		t.Fatal("expected --kernel to be required once a firecracker tier is present")
	}
	// The single tier derived from scalar flags is a Firecracker tier.
	if _, err := FromFlags([]string{"--url", "u", "--token", "t", "--golden", "/g"}); err == nil {
		t.Fatal("expected --kernel to be required without a tier catalog")
	}
}

func TestFromFlags_EC2WarmPoolOutsideSlotBudget(t *testing.T) {
	// EC2 warm runners do not count against the Firecracker slot budget.
	p := writeTiers(t, `[
	  {"name":"local","vcpu":2,"mem_mib":4096,"golden":"/g","min":2,"max":2},
	  `+strings.Replace(ec2TierJSON, `"min":1`, `"min":10`, 1)+`
	]`)
	if _, err := FromFlags(append(append(baseArgs(), jitAddr...), "--tiers", p, "--max-runners", "2")); err != nil {
		t.Fatalf("EC2 min should not count against --max-runners: %v", err)
	}
}

func TestFromFlags_EC2TierErrors(t *testing.T) {
	// Each case replaces one fragment of the valid ec2TierJSON.
	cases := map[string][2]string{
		"unknown provisioner":  {`"provisioner":"ec2"`, `"provisioner":"lambda"`},
		"missing ec2 block":    {`"ec2":{`, `"x":{`},
		"vcpu on ec2":          {`"min":1`, `"min":1,"vcpu":4`},
		"mem_mib on ec2":       {`"min":1`, `"min":1,"mem_mib":8192`},
		"golden on ec2":        {`"min":1`, `"min":1,"golden":"/g"`},
		"toolcache on ec2":     {`"min":1`, `"min":1,"toolcache":"/tc"`},
		"no instance types":    {`["c8a.xlarge","c7a.xlarge"]`, `[]`},
		"blank instance type":  {`["c8a.xlarge","c7a.xlarge"]`, `["c8a.xlarge",""]`},
		"dup instance type":    {`["c8a.xlarge","c7a.xlarge"]`, `["c8a.xlarge","c8a.xlarge"]`},
		"no ami":               {`"ami":"/firerunner/ami/runner"`, `"ami":""`},
		"bare ami prefix":      {`"ami":"/firerunner/ami/runner"`, `"ami":"ami-"`},
		"no subnets":           {`["subnet-aaaa","subnet-bbbb"]`, `[]`},
		"dup subnet":           {`["subnet-aaaa","subnet-bbbb"]`, `["subnet-aaaa","subnet-aaaa"]`},
		"no security groups":   {`["sg-cccc"]`, `[]`},
		"no market":            {`"market":"spot-then-on-demand",`, ``},
		"bad market":           {`"market":"spot-then-on-demand"`, `"market":"reserved"`},
		"negative root volume": {`"root_volume_gib":40`, `"root_volume_gib":-1`},
		"negative pool":        {`"root_volume_gib":40`, `"root_volume_gib":40,"pool":-1`},
		"bad volume type":      {`"root_volume_gib":40`, `"root_volume_gib":40,"root_volume_type":"st1"`},
		"io2 needs iops":       {`"root_volume_gib":40`, `"root_volume_gib":40,"root_volume_type":"io2"`},
		"zero max":             {`"max":20`, `"max":0`},
		"min over max":         {`"min":1`, `"min":30`},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(ec2TierJSON, r[0], r[1], 1)
			if body == ec2TierJSON {
				t.Fatalf("fragment %q not found; the case tests nothing", r[0])
			}
			p := writeTiers(t, `[`+body+`]`)
			_, err := FromFlags(append(append(baseArgs(), jitAddr...), "--tiers", p))
			if err == nil {
				t.Fatalf("expected error for %q", name)
			}
			if strings.Contains(err.Error(), "--jit-addr") {
				t.Fatalf("%q failed on the JIT flag, not the tier: %v", name, err)
			}
		})
	}
}

func TestFromFlags_EC2BlockOnFirecrackerTier(t *testing.T) {
	// An ec2 block without "provisioner":"ec2" is a mixed tier and is rejected
	// rather than silently booting a microVM.
	for name, body := range map[string]string{
		"implicit firecracker": `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":2,"ec2":{"instance_types":["c8a.xlarge"]}}]`,
		"explicit firecracker": `[{"name":"t","provisioner":"firecracker","vcpu":2,"mem_mib":4096,"golden":"/g","max":2,"ec2":{}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := writeTiers(t, body)
			_, err := FromFlags(append(append(baseArgs(), jitAddr...), "--tiers", p))
			if err == nil || !strings.Contains(err.Error(), "ec2 block") {
				t.Fatalf("err = %v, want an ec2-block error", err)
			}
		})
	}
}

func TestFromFlags_ExplicitFirecrackerProvisioner(t *testing.T) {
	// "provisioner":"firecracker" is the same as leaving it out.
	p := writeTiers(t, `[{"name":"t","provisioner":"firecracker","vcpu":2,"mem_mib":4096,"golden":"/g","max":2}]`)
	c, err := FromFlags(append(baseArgs(), "--tiers", p))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Tiers[0].IsEC2() {
		t.Fatal("explicit firecracker tier reported as ec2")
	}
}

func TestFromFlags_JITAddr(t *testing.T) {
	ec2Only := writeTiers(t, `[`+ec2TierJSON+`]`)
	args := func(extra ...string) []string {
		return append([]string{"--url", "u", "--token", "t", "--tiers", ec2Only}, extra...)
	}
	c, err := FromFlags(args("--jit-addr", "10.0.0.5:8098"))
	if err != nil {
		t.Fatalf("valid --jit-addr: %v", err)
	}
	if c.JITAddr != "10.0.0.5:8098" || c.JITTTL != 5*time.Minute {
		t.Fatalf("JITAddr=%q JITTTL=%s, want the address and the 5m default", c.JITAddr, c.JITTTL)
	}
	if _, err := FromFlags(args()); err == nil || !strings.Contains(err.Error(), "--jit-addr is required") {
		t.Fatalf("ec2 tier without --jit-addr: err = %v", err)
	}
	for name, addr := range map[string]string{
		"hostname":   "control.internal:8098",
		"no port":    "10.0.0.5",
		"zero port":  "10.0.0.5:0",
		"wildcard":   "0.0.0.0:8098",
		"loopback":   "127.0.0.1:8098",
		"link-local": "169.254.169.254:80",
		"v6 any":     "[::]:8098",
	} {
		if _, err := FromFlags(args("--jit-addr", addr)); err == nil {
			t.Errorf("%s: --jit-addr %q accepted", name, addr)
		}
	}
	if _, err := FromFlags(args("--jit-addr", "10.0.0.5:8098", "--jit-ttl", "5s")); err == nil {
		t.Error("--jit-ttl below 30s accepted")
	}
	// Without an ec2 tier the flag is optional and its absence is fine.
	if _, err := FromFlags(baseArgs()); err != nil {
		t.Fatalf("firecracker-only config without --jit-addr: %v", err)
	}
	t.Setenv("FR_JIT_ADDR", "10.0.0.6:9000")
	t.Setenv("FR_JIT_TTL", "2m")
	c, err = FromFlags(args())
	if err != nil || c.JITAddr != "10.0.0.6:9000" || c.JITTTL != 2*time.Minute {
		t.Fatalf("env fallback: c=%+v err=%v", c, err)
	}
}

func TestExampleEC2Catalog(t *testing.T) {
	// The README points at this file; keep it loadable and valid.
	c, err := FromFlags(append(append(baseArgs(), jitAddr...), "--tiers", "../../examples/tiers-ec2.json", "--max-runners", "8"))
	if err != nil {
		t.Fatalf("examples/tiers-ec2.json: %v", err)
	}
	if !c.UsesEC2() || !c.UsesFirecracker() {
		t.Fatalf("example should mix both provisioners: ec2=%v firecracker=%v", c.UsesEC2(), c.UsesFirecracker())
	}
}

func TestCheckCatalog(t *testing.T) {
	// The example catalogs in the repo pass on their own, without any of the
	// daemon's credentials or host flags.
	for _, p := range []string{"../../examples/tiers.json", "../../examples/tiers-ec2.json"} {
		tiers, err := CheckCatalog(p, 8)
		if err != nil || len(tiers) == 0 {
			t.Fatalf("CheckCatalog(%s) = %d tiers, %v", p, len(tiers), err)
		}
	}
	// Per-tier rules still apply, including the slot budget.
	bad := writeTiers(t, `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":8}]`)
	if _, err := CheckCatalog(bad, 4); err == nil || !strings.Contains(err.Error(), "--max-runners") {
		t.Fatalf("over-budget tier: err = %v", err)
	}
	mixed := writeTiers(t, `[`+strings.Replace(ec2TierJSON, `"min":1`, `"min":1,"vcpu":4`, 1)+`]`)
	if _, err := CheckCatalog(mixed, 4); err == nil {
		t.Fatal("mixed-field tier accepted")
	}
	if _, err := CheckCatalog(filepath.Join(t.TempDir(), "none.json"), 4); err == nil {
		t.Fatal("missing file accepted")
	}
}

// captureStderr runs f and returns what it wrote to os.Stderr, where the flag
// package prints usage.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestParse_NeverEchoesCredentials(t *testing.T) {
	// The flag package prints every flag's default on -h and on any parse
	// error; credentials from the env must not be among them.
	t.Setenv("FR_TOKEN", "ghp_SECRET")
	t.Setenv("FR_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----SECRET")
	for _, args := range [][]string{{"-h"}, {"--bogus"}, {"--vcpu", "x"}} {
		out := captureStderr(t, func() { _, _ = Parse(args) })
		if strings.Contains(out, "SECRET") {
			t.Fatalf("%v printed a credential:\n%s", args, out)
		}
		if !strings.Contains(out, "FR_TOKEN") {
			t.Fatalf("%v: usage should still name the env var:\n%s", args, out)
		}
	}
	// The env still supplies them, and a flag still wins.
	c, err := Parse(nil)
	if err != nil || c.Token != "ghp_SECRET" || !strings.Contains(c.AppPrivateKey, "BEGIN") {
		t.Fatalf("env fallback: %+v %v", c, err)
	}
	c, err = Parse([]string{"--token", "flag-token"})
	if err != nil || c.Token != "flag-token" {
		t.Fatalf("flag override: %q %v", c.Token, err)
	}
}
