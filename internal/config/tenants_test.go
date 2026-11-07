package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/schedule"
)

func tenantConfig(tenants ...Tenant) *Config {
	return &Config{
		URL: "https://github.com/home", AppClientID: "Iv1", Tenants: tenants,
		Tiers: []Tier{
			{Name: "cloud", Provisioner: ProvisionerEC2, Min: 2, Schedule: &schedule.Schedule{},
				EC2: &EC2Tier{Cache: true, GitCache: true, Pool: 3, PoolFor: &PoolFor{}}},
			{Name: "micro"},
		},
	}
}

func goodTenant() Tenant {
	return Tenant{ID: "t1", URL: "https://github.com/acme", InstallationID: 7, Tiers: []string{"cloud"}}
}

func TestValidateTenants(t *testing.T) {
	bad := map[string]func(*Tenant){
		"id with capitals":      func(x *Tenant) { x.ID = "T1" },
		"no id":                 func(x *Tenant) { x.ID = "" },
		"no url":                func(x *Tenant) { x.URL = " " },
		"the install's own url": func(x *Tenant) { x.URL = "https://GitHub.com/home/" },
		"a repo of the install": func(x *Tenant) { x.URL = "https://github.com/home/repo" },
		"plain http":            func(x *Tenant) { x.URL = "http://github.com/acme" },
		"no account":            func(x *Tenant) { x.URL = "https://github.com/" },
		"no installation":       func(x *Tenant) { x.InstallationID = 0 },
		"no tiers":              func(x *Tenant) { x.Tiers = nil },
		"an unknown tier":       func(x *Tenant) { x.Tiers = []string{"nope"} },
		"a tier twice":          func(x *Tenant) { x.Tiers = []string{"cloud", "cloud"} },
		"a firecracker tier":    func(x *Tenant) { x.Tiers = []string{"micro"} },
	}
	if err := tenantConfig(goodTenant()).validateTenants(); err != nil {
		t.Fatalf("a good tenant: %v", err)
	}
	for name, change := range bad {
		x := goodTenant()
		change(&x)
		if err := tenantConfig(x).validateTenants(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	second := goodTenant()
	second.ID = "t2"
	if err := tenantConfig(goodTenant(), second).validateTenants(); err == nil {
		t.Error("two tenants on one url: accepted")
	}
	second = goodTenant()
	second.URL = "https://github.com/other"
	if err := tenantConfig(goodTenant(), second).validateTenants(); err == nil {
		t.Error("one id twice: accepted")
	}
	c := tenantConfig(goodTenant())
	c.Owner = "fr/t1"
	if err := c.validateTenants(); err == nil {
		t.Error("an owner that reads as a tenant's: accepted")
	}
	c = tenantConfig(goodTenant())
	c.AppClientID = ""
	if err := c.validateTenants(); err == nil {
		t.Error("tenants on a token: accepted")
	}
}

// The cache has no boundary between tenants yet, and a tenant's tier must not
// change the catalog's own.
func TestTenantTiersHaveNoCacheOrIdleMachines(t *testing.T) {
	c := tenantConfig()
	got := c.TenantTiers(goodTenant())
	if len(got) != 1 || got[0].Name != "cloud" {
		t.Fatalf("tiers = %+v", got)
	}
	if got[0].EC2.Cache || got[0].EC2.GitCache {
		t.Fatalf("a tenant's tier caches: %+v", got[0].EC2)
	}
	if got[0].Min != 0 || got[0].Schedule != nil || got[0].EC2.Pool != 0 || got[0].EC2.PoolFor != nil {
		t.Fatalf("a tenant's tier keeps machines idle: %+v, %+v", got[0], got[0].EC2)
	}
	own := c.Tiers[0]
	if !own.EC2.Cache || !own.EC2.GitCache || own.EC2.Pool != 3 || own.Min != 2 {
		t.Fatalf("the catalog's own tier changed: %+v, %+v", own, own.EC2)
	}
}

func TestLoadTenants(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "tenants.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := loadTenants(write(`[{"id":"t1","url":"https://github.com/acme","installation_id":7,"tiers":["cloud"]}]`))
	if err != nil || len(got) != 1 || got[0].InstallationID != 7 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := loadTenants(write(`[{"id":"t1","installation":7}]`)); err == nil || !strings.Contains(err.Error(), "installation") {
		t.Fatalf("a misspelt field: %v", err)
	}
	if _, err := loadTenants(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file: no error")
	}
}
