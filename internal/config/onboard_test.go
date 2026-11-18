package config

import (
	"slices"
	"strings"
	"testing"
)

// Hosted onboarding writes the tenants table the leases serve, only from
// signed deliveries, with tiers for new tenants, into a group of its own.
func TestCheckOnboard(t *testing.T) {
	good := func() *Config {
		return &Config{OnboardQueueURL: "https://sqs.us-east-1.amazonaws.com/000000000000/onboard", TenantLeases: true,
			OnboardWebhookSecretFile: "/etc/firerunner/webhook-secret", OnboardTiers: []string{"cloud"}, OnboardRunnerGroup: "firerunner"}
	}
	if err := good().checkOnboard(); err != nil {
		t.Fatalf("good: %v", err)
	}
	if err := (&Config{}).checkOnboard(); err != nil {
		t.Fatalf("off: %v", err)
	}
	for name, c := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"plain http":    {func(c *Config) { c.OnboardQueueURL = "http://sqs/x" }, "https"},
		"no leases":     {func(c *Config) { c.TenantLeases = false }, "--tenant-leases"},
		"no secret":     {func(c *Config) { c.OnboardWebhookSecretFile = "" }, "--onboard-webhook-secret-file"},
		"no tiers":      {func(c *Config) { c.OnboardTiers = nil }, "--onboard-tiers"},
		"default group": {func(c *Config) { c.OnboardRunnerGroup = "Default" }, "default"},
		"no group":      {func(c *Config) { c.OnboardRunnerGroup = " " }, "--onboard-runner-group"},
	} {
		c0 := good()
		c.change(c0)
		if err := c0.checkOnboard(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// The flags parse, the tiers split on commas.
func TestParseOnboard(t *testing.T) {
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://u@h/db")
	p, err := Parse([]string{"--url", "https://github.com/o", "--token", "t", "--tenants", "db:", "--owner", "vm-1", "--tenant-owner", "fr", "--tenant-leases",
		"--onboard-queue-url", "https://sqs.us-east-1.amazonaws.com/000000000000/onboard", "--onboard-webhook-secret-file", "/s", "--onboard-tiers", "a, b,,"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.OnboardTiers, []string{"a", "b"}) || p.OnboardRunnerGroup != "firerunner" {
		t.Fatalf("tiers %q, group %q", p.OnboardTiers, p.OnboardRunnerGroup)
	}
}
