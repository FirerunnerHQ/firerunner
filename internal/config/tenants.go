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

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
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
	// RunnerGroupID, when set, is the one runner group its scale sets are
	// registered in, by id, whatever group RunnerGroup names: onboarding
	// sets it to the group it created and keeps, so that a group of the
	// same name the organization made itself is never used. 0 is a group
	// not ready yet (or refused): the tenant is not served until it is.
	// Left out, the group is found by RunnerGroup's name.
	RunnerGroupID *int64 `json:"runner_group_id,omitempty"`
	// Limits bound what it runs; left out, it gets the defaults
	// (EffectiveLimits).
	Limits *TenantLimits `json:"limits,omitempty"`
}

var tenantID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// TenantsSSM is the prefix of a --tenants that names an SSM parameter
// holding the list, rather than a file.
const TenantsSSM = "ssm:"

// TenantsParameter returns the SSM parameter spec names, and whether it
// names one.
func TenantsParameter(spec string) (string, bool) {
	return strings.CutPrefix(spec, TenantsSSM)
}

// TenantsDB is the --tenants that reads the list from the tenants table of
// the job store's database (FR_JOB_EVENTS_DSN), one row a tenant.
const TenantsDB = "db:"

// TenantsFromFile reports whether spec names a file, read at start, rather
// than a store the daemon reads in its own time.
func TenantsFromFile(spec string) bool {
	_, ssm := TenantsParameter(spec)
	return spec != "" && !ssm && !strings.HasPrefix(spec, "db:")
}

func loadTenants(path string) ([]Tenant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--tenants: %w", err)
	}
	tenants, err := ParseTenants(b)
	if err != nil {
		return nil, fmt.Errorf("--tenants %s: %w", path, err)
	}
	return tenants, nil
}

// ParseTenants reads a tenants list: a JSON array, nothing after it, and no
// field this version does not know.
func ParseTenants(b []byte) ([]Tenant, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var tenants []Tenant
	if err := dec.Decode(&tenants); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("content after the list")
	}
	return tenants, nil
}

// checkEntitlements reports whether --entitlements names a store: db:, which
// needs the job store, ssm: and an SSM path, or an absolute file path.
func (c *Config) checkEntitlements() error {
	switch e := c.Entitlements; {
	case e == "":
		return nil
	case e == TenantsDB:
		if c.JobEventsDSN == "" && c.JobEventsDSNFile == "" {
			return errors.New("--entitlements db: reads the job store's database: set FR_JOB_EVENTS_DSN or FR_JOB_EVENTS_DSN_FILE")
		}
		return nil
	case strings.HasPrefix(e, TenantsDB):
		return fmt.Errorf("--entitlements %q: the database is the job store's; write db: alone", e)
	}
	if err := tenantstate.CheckSpec(c.Entitlements); err != nil {
		return fmt.Errorf("--entitlements %w", err)
	}
	return nil
}

func (c *Config) checkTenantsSpec() error {
	if strings.HasPrefix(c.TenantsPath, "db:") && c.TenantsPath != TenantsDB {
		return fmt.Errorf("--tenants %q: the database is the job store's; write db: alone", c.TenantsPath)
	}
	if c.TenantsPath == TenantsDB && c.JobEventsDSN == "" && c.JobEventsDSNFile == "" {
		return errors.New("--tenants db: reads the job store's database: set FR_JOB_EVENTS_DSN or FR_JOB_EVENTS_DSN_FILE")
	}
	if o := c.TenantOwner; o != "" && (len(o) > 64 || strings.HasPrefix(strings.ToLower(o), "aws:") || strings.TrimSpace(o) != o || strings.Contains(o, "/")) {
		// A / would make <it>/<id> read as another owner's tenant.
		return fmt.Errorf("--tenant-owner %q must be at most 64 characters, not start with aws:, have no surrounding spaces and no /", o)
	}
	if c.TenantLeases && c.TenantsPath != TenantsDB {
		return errors.New("--tenant-leases keeps the leases beside the tenants table: set --tenants db:")
	}
	if c.TenantLeases && (c.Owner == "" || c.TenantOwner == "") {
		// A tenant handed to another shard leaves it its busy runners, which
		// that shard's reaper finds by the tenant's owner tag; a shard's own
		// tiers and pool are its alone, by its --owner.
		return errors.New("--tenant-leases needs --tenant-owner, the same on every shard, by which a tenant's busy runners are handed to the next one; and --owner, a different one on each")
	}
	if c.TenantLeases && c.UsesEC2() && c.EC2PoolKeyFile == "" {
		// A process's own random key proves nothing to the next shard,
		// which would leave the runners handed to it to their max lifetime.
		return errors.New("--tenant-leases with EC2 tiers needs --ec2-pool-key-file, the same on every shard: the next shard proves by it that the runners handed to it are its to end")
	}
	name, ok := TenantsParameter(c.TenantsPath)
	if !ok {
		return nil
	}
	if !strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("--tenants %q: an SSM parameter name is absolute, e.g. ssm:/firerunner/control/tenants", c.TenantsPath)
	}
	return nil
}

func (c *Config) validateTenants() error {
	if err := c.checkTenantsSpec(); err != nil {
		return err
	}
	return c.CheckTenants(c.Tenants)
}

// CheckTenants reports what is wrong with a tenants list for this install:
// the list from a file is checked at start, and every list the daemon reads
// again before it serves any of it.
func (c *Config) CheckTenants(tenants []Tenant) error {
	if len(tenants) == 0 {
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
	for _, t := range tenants {
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
		if g := t.RunnerGroupID; g != nil && (*g < 0 || *g == 1) {
			// 1 is every organization's default group, open to every
			// repository, public ones too.
			return fmt.Errorf("tenant %q: runner_group_id %d: want a group's id, never the default's (1), or 0 for one not ready", t.ID, *g)
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
		if t.Limits != nil {
			for name, n := range t.Limits.Pool {
				tier := catalog[name]
				switch {
				case !seen[name]:
					return fmt.Errorf("tenant %q: limits.pool names tier %q, which it is not given", t.ID, name)
				case n < 0:
					return fmt.Errorf("tenant %q: limits.pool %q is %d: want 0 or more", t.ID, name, n)
				case tier.Max > 0 && n > tier.Max:
					return fmt.Errorf("tenant %q: limits.pool %q is %d, above the tier's max of %d", t.ID, name, n, tier.Max)
				}
				if _, refused := withinVCPU([]Tier{tier}, t.EffectiveLimits().VCPU); n > 0 && len(refused) > 0 {
					return fmt.Errorf("tenant %q: limits.pool %q: the tier is not served to it (%s)", t.ID, name, refused[0])
				}
			}
		}
		if err := t.EffectiveLimits().validate(fmt.Sprintf("tenant %q: ", t.ID), catalogLimitNames); err != nil {
			return err
		}
	}
	return nil
}

// AccountOf returns the account a runner URL registers runners in, as
// accountOf: two tenants on one account would share scale sets.
func AccountOf(raw string) string { return accountOf(raw) }

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
// keep no hot runners, nor the catalog's pool or schedule, which would multiply
// by the tenants however idle they are: a tier's pool is the one its entry
// sets (limits.pool), and none by default.
func (c *Config) TenantTiers(t Tenant) ([]Tier, []string) {
	var tiers []Tier
	for _, name := range t.Tiers {
		for _, tier := range c.Tiers {
			if tier.Name != name {
				continue
			}
			e := *tier.EC2
			e.Cache, e.GitCache = false, false
			e.Pool, e.PoolFor = t.PoolSize(name), nil
			tier.EC2, tier.Min, tier.Schedule = &e, 0, nil
			tiers = append(tiers, tier)
		}
	}
	return withinVCPU(tiers, t.EffectiveLimits().VCPU)
}
