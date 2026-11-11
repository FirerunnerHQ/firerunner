package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Tenant is an installation a shard serves beside the one the flags name. It
// uses the install's GitHub App and tier catalog, with its own installation,
// owner tag and scale sets.
type Tenant struct {
	// ID names the tenant in logs and health, and suffixes its owner tag.
	ID string `json:"id"`
	// URL is the organization or repository its runners register to.
	URL string `json:"url"`
	// InstallationID is the App's installation there.
	InstallationID int64 `json:"installation_id"`
	// Tiers names the catalog tiers it gets.
	Tiers []string `json:"tiers"`
	// RunnerGroup defaults to the install's --runner-group.
	RunnerGroup string `json:"runner_group,omitempty"`
	// Limits bound what it runs; left out, it gets the defaults
	// (EffectiveLimits).
	Limits *TenantLimits `json:"limits,omitempty"`
}

var tenantID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func loadTenants(path string) ([]Tenant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--tenants: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var tenants []Tenant
	if err := dec.Decode(&tenants); err != nil {
		return nil, fmt.Errorf("--tenants %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("--tenants %s: content after the list", path)
	}
	return tenants, nil
}

func (c *Config) validateTenants() error {
	if len(c.Tenants) == 0 {
		return nil
	}
	if c.AppClientID == "" || (c.AppPrivateKey == "" && c.AppKMSKey == "") {
		return errors.New("--tenants needs a GitHub App's client id and key: a token reaches one organization")
	}
	if strings.Contains(c.Owner, "/") {
		return fmt.Errorf("--owner %q: with --tenants it cannot hold a slash, which starts a tenant's owner tag", c.Owner)
	}
	catalog := map[string]Tier{}
	for _, t := range c.Tiers {
		catalog[t.Name] = t
	}
	ids, urls := map[string]bool{}, map[string]bool{accountOf(c.URL): true}
	for _, t := range c.Tenants {
		if !tenantID.MatchString(t.ID) {
			return fmt.Errorf("tenant %q: an id is lowercase letters, digits and hyphens, at most 32", t.ID)
		}
		if ids[t.ID] {
			return fmt.Errorf("tenant %q: listed twice", t.ID)
		}
		ids[t.ID] = true
		// Two sessions on one scale set would fight over it.
		u := accountOf(t.URL)
		if u == "" || urls[u] {
			return fmt.Errorf("tenant %q: url %q is not an https URL of an account not yet served", t.ID, t.URL)
		}
		urls[u] = true
		if t.InstallationID <= 0 {
			return fmt.Errorf("tenant %q: installation_id is required", t.ID)
		}
		if len(t.Tiers) == 0 {
			return fmt.Errorf("tenant %q: tiers is required", t.ID)
		}
		seen := map[string]bool{}
		for _, name := range t.Tiers {
			tier, ok := catalog[name]
			switch {
			case !ok:
				return fmt.Errorf("tenant %q: tier %q is not in the catalog", t.ID, name)
			case seen[name]:
				return fmt.Errorf("tenant %q: tier %q is listed twice", t.ID, name)
			case !tier.IsEC2():
				// Firecracker tiers share one host and its slots.
				return fmt.Errorf("tenant %q: tier %q is not an EC2 tier", t.ID, name)
			}
			seen[name] = true
		}
		if err := t.EffectiveLimits().validate(fmt.Sprintf("tenant %q: ", t.ID), catalogLimitNames); err != nil {
			return err
		}
	}
	return nil
}

// accountOf returns the host and organization of a runner URL, or "" if it is
// not an https URL naming one. A repository in an organization counts as that
// organization, whose runners its jobs could also reach.
func accountOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	org, _, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
	if org == "" {
		return ""
	}
	return strings.ToLower(u.Host + "/" + org)
}

// TenantTiers returns the catalog tiers t gets, and why it leaves out any it
// lists: a tier above the tenant's vCPU limit is not served to it. Their caches
// are off, as the cache has no boundary between tenants yet (#168), and they
// keep no hot runners or pool, which would multiply by the tenants however idle
// they are.
func (c *Config) TenantTiers(t Tenant) ([]Tier, []string) {
	var tiers []Tier
	for _, name := range t.Tiers {
		for _, tier := range c.Tiers {
			if tier.Name != name {
				continue
			}
			e := *tier.EC2
			e.Cache, e.GitCache = false, false
			e.Pool, e.PoolFor = 0, nil
			tier.EC2, tier.Min, tier.Schedule = &e, 0, nil
			tiers = append(tiers, tier)
		}
	}
	return withinVCPU(tiers, t.EffectiveLimits().VCPU)
}
