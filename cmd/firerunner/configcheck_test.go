package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
)

func runConfig(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := configCmd(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestConfigCheckCatalog(t *testing.T) {
	t.Setenv("FR_MAX_RUNNERS", "")
	code, out, _ := runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json", "--max-runners", "8")
	if code != 0 || !strings.Contains(out, "valid (catalog): 2 tier(s)") || !strings.Contains(out, "c8a.xlarge") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// An over-budget catalog is invalid, exit 1, and the message names the fix.
	code, out, _ = runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json", "--max-runners", "2")
	if code != 1 || !strings.Contains(out, "--max-runners") {
		t.Fatalf("over budget: exit %d\n%s", code, out)
	}
	// The budget defaults like the daemon's, from FR_MAX_RUNNERS.
	t.Setenv("FR_MAX_RUNNERS", "8")
	if code, out, _ := runConfig(t, "check", "--catalog=../../examples/tiers-ec2.json"); code != 0 {
		t.Fatalf("FR_MAX_RUNNERS default: exit %d\n%s", code, out)
	}
}

func TestConfigCheckJSON(t *testing.T) {
	code, out, _ := runConfig(t, "check", "--json", "--catalog", "../../examples/tiers-ec2.json", "--max-runners", "8")
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != 0 || !r.OK || r.Mode != "catalog" || len(r.Tiers) != 2 || r.Tiers[1].Provisioner != "ec2" || len(r.Notes) == 0 {
		t.Fatalf("report = %+v (exit %d)", r, code)
	}
	code, out, _ = runConfig(t, "check", "--json", "--catalog", "/no/such.json")
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 1 || r.OK || r.Error == "" {
		t.Fatalf("missing file: exit %d report %+v err %v", code, r, err)
	}
}

func TestConfigCheckDaemonNeverPrintsSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"check", "--url", "https://github.com/o", "--token", "ghp_SECRET", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"},
		{"check", "--json", "--url", "https://github.com/o", "--token", "ghp_SECRET", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"},
	} {
		code, out, errOut := runConfig(t, args...)
		if code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		if strings.Contains(out+errOut, "ghp_SECRET") {
			t.Fatalf("token printed:\n%s", out)
		}
		if !strings.Contains(out, "token") {
			t.Fatalf("auth kind missing:\n%s", out)
		}
	}
	// A config the daemon would refuse is invalid here too.
	code, out, _ := runConfig(t, "check", "--token", "t", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0")
	if code != 1 || !strings.Contains(out, "--url is required") {
		t.Fatalf("missing url: exit %d\n%s", code, out)
	}
}

func TestConfigCheckUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"lint"},
		{"check", "--catalog"},
		{"check", "--catalog", "x.json", "--url", "u"},
		{"check", "--catalog", "x.json", "--max-runners", "many"},
		{"check", "--catalog", "x.json", "--max-runners", "0"},
		{"check", "--bogus"},
		{"check", "--vcpu", "x"},
	} {
		if code, _, _ := runConfig(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, out, _ := runConfig(t, "check", "--help"); code != 0 || !strings.Contains(out, "exit status") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
}

func TestConfigCheckMatchesStartup(t *testing.T) {
	// A malformed FR_MAX_RUNNERS fails the daemon at startup; the catalog
	// check must not silently fall back to 4.
	t.Setenv("FR_MAX_RUNNERS", "abc")
	if code, _, errOut := runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json"); code != 2 || !strings.Contains(errOut, "FR_MAX_RUNNERS") {
		t.Fatalf("malformed FR_MAX_RUNNERS: exit %d %s", code, errOut)
	}
	t.Setenv("FR_MAX_RUNNERS", "")
	if code, out, _ := runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json", "--max-runners=8"); code != 0 {
		t.Fatalf("--max-runners=8: exit %d\n%s", code, out)
	}
	// Startup resolves the App key; a key file that does not exist is
	// invalid here too, and the message never contains the key's value.
	// (An empty FR_MAX_RUNNERS is itself malformed for the daemon.)
	t.Setenv("FR_MAX_RUNNERS", "4")
	code, out, _ := runConfig(t, "check", "--url", "https://github.com/o", "--app-client-id", "Iv1",
		"--app-installation-id", "1", "--app-private-key", "/no/such/key.pem", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0")
	if code != 1 || !strings.Contains(out, "does not exist") {
		t.Fatalf("missing app key: exit %d\n%s", code, out)
	}
}

// emptyKeyArgs is a daemon config whose App private key file is empty, the
// input that panicked the daemon in issue #78.
func emptyKeyArgs(t *testing.T) (args []string, keyPath string) {
	t.Helper()
	t.Setenv("FR_MAX_RUNNERS", "4")
	t.Setenv("FR_TOKEN", "")
	keyPath = filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--url", "https://github.com/o", "--app-client-id", "Iv1", "--app-installation-id", "1",
		"--app-private-key", keyPath, "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"}, keyPath
}

func TestConfigCheckRejectsEmptyAppKey(t *testing.T) {
	args, keyPath := emptyKeyArgs(t)
	want := "--app-private-key " + keyPath + ": file is empty"

	code, out, _ := runConfig(t, append([]string{"check"}, args...)...)
	if code != 1 || !strings.HasPrefix(out, "invalid (daemon): "+want) {
		t.Fatalf("text: exit %d\n%s", code, out)
	}
	code, out, _ = runConfig(t, append([]string{"check", "--json"}, args...)...)
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 1 || r.OK || !strings.Contains(r.Error, want) || r.Auth != "" {
		t.Fatalf("json: exit %d report %+v err %v", code, r, err)
	}
}

func TestConfigCheckRejectsAShortPoolKey(t *testing.T) {
	t.Setenv("FR_MAX_RUNNERS", "4")
	t.Setenv("FR_TOKEN", "")
	keyPath := filepath.Join(t.TempDir(), "pool-key")
	if err := os.WriteFile(keyPath, []byte("SECRETSHORT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"check", "--url", "https://github.com/o", "--token", "ghp_x", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"}
	code, out, _ := runConfig(t, append(args, "--ec2-pool-key-file", keyPath)...)
	if code != 1 || !strings.Contains(out, "--ec2-pool-key-file") || strings.Contains(out, "SECRETSHORT") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, out, _ := runConfig(t, args...); code != 0 {
		t.Fatalf("without the key file: exit %d\n%s", code, out)
	}
}

// TestRunRejectsEmptyAppKey drives the daemon's startup path: an empty key
// file is an error naming the file, never a panic in the scale set client.
func TestRunRejectsEmptyAppKey(t *testing.T) {
	args, keyPath := emptyKeyArgs(t)
	cfg, err := config.FromFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = run(ctx, cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "--app-private-key "+keyPath+": file is empty") {
		t.Fatalf("run: %v", err)
	}
}

func TestConfigCheckUnknownFlagNeverPrintsSecrets(t *testing.T) {
	t.Setenv("FR_TOKEN", "ghp_SECRET")
	t.Setenv("FR_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----SECRET")
	out := captureStderrMain(t, func() {
		var o, e bytes.Buffer
		if code := configCmd([]string{"check", "--json", "--bogus"}, &o, &e); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
		if strings.Contains(o.String()+e.String(), "SECRET") {
			t.Errorf("credential in output:\n%s%s", o.String(), e.String())
		}
	})
	if strings.Contains(out, "SECRET") {
		t.Fatalf("credential on stderr:\n%s", out)
	}
}

// captureStderrMain returns what f wrote to os.Stderr, where the flag
// package prints usage.
func captureStderrMain(t *testing.T, f func()) string {
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

// writeTenantConfig writes an EC2 catalog with a 2-vCPU and a 4-vCPU tier, and
// the tenants given, and returns the daemon flags that serve them.
func writeTenantConfig(t *testing.T, tenants string) []string {
	t.Helper()
	dir := t.TempDir()
	tier := func(name, itype string) string {
		return `{"name":"` + name + `","provisioner":"ec2","max":4,"ec2":{"instance_types":["` + itype + `"],"ami":"ami-0aaaaaaaaaaaaaaaa","subnets":["subnet-0aaaaaaaaaaaaaaaa"],"security_groups":["sg-0aaaaaaaaaaaaaaaa"],"market":"on-demand"}}`
	}
	tiers := filepath.Join(dir, "tiers.json")
	if err := os.WriteFile(tiers, []byte("["+tier("small", "c8a.large")+","+tier("big", "c8a.xlarge")+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	tenantsPath := filepath.Join(dir, "tenants.json")
	if err := os.WriteFile(tenantsPath, []byte(tenants), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--url", "https://github.com/home", "--app-client-id", "Iv1", "--app-installation-id", "1",
		"--app-kms-key", "alias/github-app", "--tiers", tiers, "--tenants", tenantsPath, "--jit-addr", "10.0.0.5:8098"}
}

// A tenant runs under the defaults unless its entry sets limits, and config
// check shows them, with a warning for each tier a vCPU limit leaves out.
func TestConfigCheckTenantLimits(t *testing.T) {
	flags := writeTenantConfig(t, `[
		{"id":"new","url":"https://github.com/new","installation_id":2,"tiers":["small","big"]},
		{"id":"raised","url":"https://github.com/raised","installation_id":3,"tiers":["small","big"],
		 "limits":{"max_runners":10,"max_vcpu":0,"max_job_duration":"2h"}},
		{"id":"tiny","url":"https://github.com/tiny","installation_id":4,"tiers":["big"],"limits":{"max_vcpu":1}}]`)
	code, out, errOut := runConfig(t, append([]string{"check", "--json"}, flags...)...)
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 {
		t.Fatalf("exit %d, %v\n%s%s", code, err, out, errOut)
	}
	if r.Limits != nil {
		t.Fatalf("the install has limits it was not given: %+v", r.Limits)
	}
	if len(r.Tenants) != 3 {
		t.Fatalf("tenants = %+v", r.Tenants)
	}
	want := []checkTenant{
		{ID: "new", Tiers: []string{"small"}, Limits: config.Limits{Runners: 2, VCPU: 2, JobDuration: 30 * time.Minute}},
		{ID: "raised", Tiers: []string{"small", "big"}, Limits: config.Limits{Runners: 10, JobDuration: 2 * time.Hour}},
		{ID: "tiny", Tiers: []string{}, Limits: config.Limits{Runners: 2, VCPU: 1, JobDuration: 30 * time.Minute}},
	}
	for i, w := range want {
		got := r.Tenants[i]
		if got.ID != w.ID || strings.Join(got.Tiers, ",") != strings.Join(w.Tiers, ",") || got.Limits.Runners != w.Limits.Runners || got.Limits.VCPU != w.Limits.VCPU {
			t.Errorf("tenant %d = %+v, want %+v", i, got, w)
		}
	}
	warnings := strings.Join(r.Warnings, "\n")
	for _, w := range []string{`tenant "new": tier "big": 4 vCPU is above the limit of 2`, `tenant "tiny": no tier is within its limits`} {
		if !strings.Contains(warnings, w) {
			t.Errorf("warnings lack %q:\n%s", w, warnings)
		}
	}
	if notes := strings.Join(r.Notes, "\n"); !strings.Contains(notes, `tenant "new" sets no limits, so it runs under the defaults: 2 runner(s)`) || strings.Contains(notes, `"raised"`) {
		t.Errorf("notes:\n%s", notes)
	}
	if strings.Contains(warnings, `"raised"`) {
		t.Errorf("a tenant with no vCPU limit is warned of:\n%s", warnings)
	}
	// The JSON carries the job limit as the catalog writes it.
	if !strings.Contains(out, `"max_job_duration": "2h0m0s"`) {
		t.Errorf("the job limit is not a duration string:\n%s", out)
	}

	// Text: the limits and the warnings, with exit 0: a warning stops nothing.
	code, out, _ = runConfig(t, append([]string{"check"}, flags...)...)
	for _, w := range []string{"3 tenant(s):", "2 runner(s), 2 vCPU per tier, 30m0s per job", `warning: tenant "new"`} {
		if code != 0 || !strings.Contains(out, w) {
			t.Errorf("exit %d, text lacks %q:\n%s", code, w, out)
		}
	}
}

// Limits that make no sense are refused, naming where they are set.
func TestConfigCheckRefusesBadLimits(t *testing.T) {
	for limits, want := range map[string]string{
		`{"max_runners":-1}`:          `tenant "t1": limits.max_runners must be >= 0`,
		`{"max_job_duration":"30s"}`:  `tenant "t1": limits.max_job_duration 30s is too short`,
		`{"max_job_duration":30}`:     `a duration is a string`,
		`{"max_job_duration":"soon"}`: `invalid duration`,
		`{"max_runner":2}`:            `unknown field "max_runner"`,
	} {
		flags := writeTenantConfig(t, `[{"id":"t1","url":"https://github.com/t1","installation_id":2,"tiers":["small"],"limits":`+limits+`}]`)
		code, out, errOut := runConfig(t, append([]string{"check"}, flags...)...)
		if code == 0 || !strings.Contains(out+errOut, want) {
			t.Errorf("%s: exit %d, want %q:\n%s%s", limits, code, want, out, errOut)
		}
	}
}

// The install's own limits come from flags, and a vCPU limit that leaves it
// nothing to serve is refused rather than serving nothing.
func TestConfigCheckInstallLimits(t *testing.T) {
	flags := writeTenantConfig(t, `[]`)
	code, out, errOut := runConfig(t, append([]string{"check", "--json", "--limit-runners", "1", "--limit-vcpu", "2", "--limit-job-duration", "2m"}, flags...)...)
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 {
		t.Fatalf("exit %d, %v\n%s%s", code, err, out, errOut)
	}
	if r.Limits == nil || *r.Limits != (config.Limits{Runners: 1, VCPU: 2, JobDuration: 2 * time.Minute}) {
		t.Fatalf("limits = %+v", r.Limits)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], `install: tier "big": 4 vCPU is above the limit of 2`) {
		t.Fatalf("warnings = %q", r.Warnings)
	}
	code, out, _ = runConfig(t, append([]string{"check", "--limit-vcpu", "1"}, flags...)...)
	if code != 1 || !strings.Contains(out, "--limit-vcpu 1 leaves the install no tier to serve") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	code, out, _ = runConfig(t, append([]string{"check", "--limit-job-duration", "10s"}, flags...)...)
	if code != 1 || !strings.Contains(out, "--limit-job-duration 10s is too short") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// Warm runners never end by themselves: a runner limit they would fill would
// hold a job on a tier without one for good.
func TestConfigCheckLimitAboveWarmRunners(t *testing.T) {
	dir := t.TempDir()
	tiers := filepath.Join(dir, "tiers.json")
	tier := func(name string, min int) string {
		return fmt.Sprintf(`{"name":"%s","provisioner":"ec2","min":%d,"max":4,"ec2":{"instance_types":["c8a.large"],"ami":"ami-0aaaaaaaaaaaaaaaa","subnets":["subnet-0aaaaaaaaaaaaaaaa"],"security_groups":["sg-0aaaaaaaaaaaaaaaa"],"market":"on-demand"}}`, name, min)
	}
	if err := os.WriteFile(tiers, []byte("["+tier("warm", 2)+","+tier("cold", 0)+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--url", "https://github.com/home", "--app-client-id", "Iv1", "--app-installation-id", "1",
		"--app-kms-key", "alias/github-app", "--tiers", tiers, "--jit-addr", "10.0.0.5:8098"}
	code, out, _ := runConfig(t, append([]string{"check", "--limit-runners", "2"}, flags...)...)
	if code != 1 || !strings.Contains(out, "--limit-runners 2 must be above the 2 warm runner(s)") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if code, out, _ := runConfig(t, append([]string{"check", "--limit-runners", "3"}, flags...)...); code != 0 {
		t.Fatalf("a limit above the warm runners: exit %d:\n%s", code, out)
	}
}
