package diag

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/firerunnerhq/firerunner/internal/awscreds"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// cacheCredsSetup is a config with --cache-role-arn, its credentials file
// expiring at exp (none when exp is zero) and the daemon's published state cc
// (none when nil).
func cacheCredsSetup(t *testing.T, now, exp time.Time, cc *health.CacheCredentials) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if !exp.IsZero() {
		if err := awscreds.Write(path, aws.Credentials{AccessKeyID: "AKIASECRETKEY", SecretAccessKey: "the-secret", SessionToken: "the-token", CanExpire: true, Expires: exp}); err != nil {
			t.Fatal(err)
		}
	}
	if cc != nil {
		cc.File = path
		f := health.File{PID: 1, WrittenAt: now, CacheCredentials: cc}
		if err := health.Write(filepath.Join(dir, health.FileName), f); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Config{CacheRoleARN: "arn:aws:iam::123456789012:role/cache", CacheCredentialsFile: path,
		Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
}

func TestCacheCredentialsCheck(t *testing.T) {
	now := time.Now()
	if c := cacheCredentialsCheck(&config.Config{}, now); c != nil {
		t.Fatalf("no role, yet a check: %+v", c)
	}
	ok := &health.CacheCredentials{LastRefresh: now.Add(-time.Minute), Expires: now.Add(59 * time.Minute)}
	recovered := &health.CacheCredentials{LastRefresh: now.Add(-time.Minute), Expires: now.Add(59 * time.Minute),
		LastError: "throttled", LastErrorAt: now.Add(-2 * time.Minute)}
	failing := &health.CacheCredentials{LastRefresh: now.Add(-20 * time.Minute), Expires: now.Add(40 * time.Minute),
		LastError: "AccessDenied: not authorized to perform sts:AssumeRole", LastErrorAt: now.Add(-time.Minute)}
	for _, c := range []struct {
		name   string
		exp    time.Time
		cc     *health.CacheCredentials
		level  string
		detail string
	}{
		{"fresh", now.Add(59 * time.Minute), ok, levelPass, "valid until"},
		{"fresh, daemon state unpublished", now.Add(59 * time.Minute), nil, levelPass, "valid until"},
		{"recovered from an error", now.Add(59 * time.Minute), recovered, levelPass, "valid until"},
		{"missing", time.Time{}, nil, levelFail, "no such file"},
		{"expired", now.Add(-time.Second), nil, levelFail, "expired"},
		{"latest refresh failed", now.Add(40 * time.Minute), failing, levelFail, "AccessDenied"},
		{"close to expiry", now.Add(10 * time.Minute), nil, levelWarn, "expires in"},
	} {
		got := cacheCredentialsCheck(cacheCredsSetup(t, now, c.exp, c.cc), now)
		if got == nil || got.Level != c.level || !strings.Contains(got.Detail, c.detail) {
			t.Errorf("%s: %+v, want %s with %q", c.name, got, c.level, c.detail)
			continue
		}
		for _, secret := range []string{"AKIASECRETKEY", "the-secret", "the-token"} {
			if strings.Contains(got.Detail, secret) {
				t.Errorf("%s: the check prints the credentials: %s", c.name, got.Detail)
			}
		}
	}
}

func TestDoctorFailsOnMissingCacheCredentials(t *testing.T) {
	cfg := cacheCredsSetup(t, time.Now(), time.Time{}, nil)
	r := runDoctor(cfg, "test")
	for _, c := range r.Checks {
		if c.Name == "cache-credentials" {
			if c.Level != levelFail || r.OK {
				t.Fatalf("missing file: %+v, ok %v", c, r.OK)
			}
			return
		}
	}
	t.Fatal("doctor has no cache-credentials check")
}

func TestStatusShowsCacheCredentials(t *testing.T) {
	now := time.Now()
	cc := &health.CacheCredentials{LastRefresh: now.Add(-time.Minute), Expires: now.Add(59 * time.Minute),
		LastError: "throttled", LastErrorAt: now.Add(-30 * time.Second)}
	cfg := cacheCredsSetup(t, now, now.Add(59*time.Minute), cc)
	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Cache credentials: " + cfg.CacheCredentialsFile, "valid until", "last refresh failed", "throttled"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("status missing %q\n%s", want, text.String())
		}
	}
	var js bytes.Buffer
	if err := Status(cfg, "test", &js, true); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Listeners struct {
			CacheCredentials map[string]any `json:"cache_credentials"`
		} `json:"listeners"`
	}
	if err := json.Unmarshal(js.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	got := r.Listeners.CacheCredentials
	for _, k := range []string{"file", "last_refresh", "expires", "last_error", "last_error_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status --json cache_credentials has no %s: %v", k, got)
		}
	}
	data, _ := os.ReadFile(cfg.CacheCredentialsFile)
	if bytes.Contains(js.Bytes(), []byte("the-secret")) || len(data) == 0 {
		t.Fatal("status --json carries the credentials")
	}
}
