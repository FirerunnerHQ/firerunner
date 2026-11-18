package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/ghauth"
	"github.com/firerunnerhq/firerunner/internal/schedule"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// The daemon builds onboarding's worker only when asked, refuses to start
// on a setting that would refuse every delivery, and works only while it
// holds the assigner's lease.
func TestNewOnboarding(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := jobstore.OpenTenants("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	now := time.Now()
	leaser := &tenantLeaser{held: map[string]time.Time{}, now: func() time.Time { return now }}
	jwt := ghauth.NewJWT("Iv1", nil)
	cfgWith := func(f func(*config.Config)) *config.Config {
		c := &config.Config{URL: "https://github.com/home", AppClientID: "Iv1", AppPrivateKey: "key.pem",
			Tiers: []config.Tier{
				{Name: "cloud", Provisioner: config.ProvisionerEC2, Schedule: &schedule.Schedule{}, EC2: &config.EC2Tier{}},
				{Name: "micro"},
			},
			OnboardQueueURL: "https://sqs.us-east-1.amazonaws.com/000000000000/onboard", OnboardWebhookSecretFile: secret,
			OnboardTiers: []string{"cloud"}, OnboardRunnerGroup: "firerunner"}
		if f != nil {
			f(c)
		}
		return c
	}
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	if w, err := newOnboarding(ctx, cfgWith(func(c *config.Config) { c.OnboardQueueURL = "" }), nil, nil, nil, log); w != nil || err != nil {
		t.Fatalf("off: %v, %v", w, err)
	}
	for name, c := range map[string]struct {
		cfg    *config.Config
		leaser *tenantLeaser
		want   string
	}{
		"no secret file":   {cfgWith(func(c *config.Config) { c.OnboardWebhookSecretFile = filepath.Join(dir, "none") }), leaser, "--onboard-webhook-secret-file"},
		"empty secret":     {cfgWith(func(c *config.Config) { c.OnboardWebhookSecretFile = empty }), leaser, "empty"},
		"firecracker tier": {cfgWith(func(c *config.Config) { c.OnboardTiers = []string{"micro"} }), leaser, "--onboard-tiers"},
		"unknown tier":     {cfgWith(func(c *config.Config) { c.OnboardTiers = []string{"nope"} }), leaser, "--onboard-tiers"},
		"no leases":        {cfgWith(nil), nil, "--tenant-leases"},
	} {
		if w, err := newOnboarding(ctx, c.cfg, db, c.leaser, jwt, log); w != nil || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, %v; want %q", name, w, err, c.want)
		}
	}

	w, err := newOnboarding(ctx, cfgWith(nil), db, leaser, jwt, log)
	if err != nil || w == nil {
		t.Fatalf("good: %v, %v", w, err)
	}
	if string(w.Config.Secret) != "s3cret" || w.Config.Reserved[0] != "github.com/home" || w.Keeper.Group != "firerunner" || w.Config.Lookup == nil || w.Config.Check == nil {
		t.Fatalf("worker %+v", w.Config)
	}
	if w.Active() {
		t.Fatal("active without the assigner's lease")
	}
	leaser.held[jobstore.AssignerLease] = now.Add(time.Second)
	if !w.Active() {
		t.Fatal("not active with the assigner's lease")
	}
	leaser.held[jobstore.AssignerLease] = now
	if w.Active() {
		t.Fatal("active at the lease's deadline")
	}
}

// The SQS client goes to the region the queue's URL names.
func TestQueueRegion(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sqs.eu-west-1.amazonaws.com/000000000000/q": "eu-west-1",
		"https://SQS.us-east-2.amazonaws.com/000000000000/q": "us-east-2",
		"https://vpce-1.sqs.us-east-1.vpce.amazonaws.com/q":  "",
		"https://queue.amazonaws.com/000000000000/q":         "",
		"https://sqs.us-east-1.vpce.amazonaws.com/q":         "",
		"%": "",
	} {
		if got := queueRegion(raw); got != want {
			t.Errorf("queueRegion(%q) = %q, want %q", raw, got, want)
		}
	}
}
