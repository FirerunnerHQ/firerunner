package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/ghauth"
	"github.com/firerunnerhq/firerunner/internal/ghrunners"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
	"github.com/firerunnerhq/firerunner/proprietary/onboard"
)

// assigning reports whether this shard holds the assigner's lease: the
// one shard that applies the hosted App's webhooks.
func (l *tenantLeaser) assigning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.assigningLocked()
}

// newOnboarding builds hosted onboarding's worker (--onboard-queue-url),
// or nil when it is off. Every setting is checked here, at start: a
// secret that cannot be read, or default tiers the catalog would not
// serve, would refuse every delivery.
func newOnboarding(ctx context.Context, cfg *config.Config, db *jobstore.TenantStore, leaser *tenantLeaser, jwt *ghauth.JWT, log *slog.Logger) (*onboard.Worker, error) {
	if cfg.OnboardQueueURL == "" {
		return nil, nil
	}
	if db == nil || leaser == nil || jwt == nil {
		return nil, errors.New("--onboard-queue-url needs --tenants db:, --tenant-leases and a GitHub App")
	}
	secret, err := os.ReadFile(cfg.OnboardWebhookSecretFile)
	if err != nil {
		return nil, fmt.Errorf("--onboard-webhook-secret-file: %w", err)
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	if len(secret) == 0 {
		return nil, errors.New("--onboard-webhook-secret-file is empty")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("--url: %w", err)
	}
	gh := onboard.NewGitHub(ghrunners.APIBase(u), jwt)
	oc := onboard.Config{Secret: secret, Tiers: cfg.OnboardTiers, RunnerGroup: cfg.OnboardRunnerGroup,
		Reserved: []string{config.AccountOf(cfg.URL)}, Check: cfg.CheckTenants, Lookup: gh.Installation}
	if err := oc.Validate(cfg); err != nil {
		return nil, fmt.Errorf("--onboard-tiers: %w", err)
	}
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &onboard.Worker{
		Queue: awssqs.NewFromConfig(awsCfg), QueueURL: cfg.OnboardQueueURL,
		Config: oc, Store: db,
		Keeper: onboard.Keeper{Store: db, GitHub: gh, Lookup: gh.Installation, Group: cfg.OnboardRunnerGroup},
		Active: leaser.assigning, Log: log,
	}, nil
}

func healthOnboarding(st onboard.WorkerStatus) *health.Onboarding {
	o := health.Onboarding(st)
	return &o
}
