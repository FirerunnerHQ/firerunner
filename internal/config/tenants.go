package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
	return tenants, nil
}

func (c *Config) validateTenants() error {
	if len(c.Tenants) == 0 {
		return nil
	}
	if c.AppClientID == "" {
		return errors.New("--tenants needs GitHub App credentials: a token reaches one organization")
	}
	catalog := map[string]Tier{}
	for _, t := range c.Tiers {
		catalog[t.Name] = t
	}
	ids, urls := map[string]bool{}, map[string]bool{normalURL(c.URL): true}
	for _, t := range c.Tenants {
		if !tenantID.MatchString(t.ID) {
			return fmt.Errorf("tenant %q: an id is lowercase letters, digits and hyphens, at most 32", t.ID)
		}
		if ids[t.ID] {
			return fmt.Errorf("tenant %q: listed twice", t.ID)
		}
		ids[t.ID] = true
		// Two sessions on one scale set would fight over it.
		if u := normalURL(t.URL); u == "" || urls[u] {
			return fmt.Errorf("tenant %q: url %q is empty or already served", t.ID, t.URL)
		} else {
			urls[u] = true
		}
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
	}
	return nil
}

func normalURL(u string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/")) }

// TenantTiers returns the catalog tiers t gets. Their caches are off: the cache
// has no boundary between tenants yet (#168).
func (c *Config) TenantTiers(t Tenant) []Tier {
	var tiers []Tier
	for _, name := range t.Tiers {
		for _, tier := range c.Tiers {
			if tier.Name != name {
				continue
			}
			e := *tier.EC2
			e.Cache, e.GitCache = false, false
			tier.EC2 = &e
			tiers = append(tiers, tier)
		}
	}
	return tiers
}
