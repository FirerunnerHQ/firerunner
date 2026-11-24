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
		URL: "https://github.com/home", AppClientID: "Iv1", AppPrivateKey: "key.pem", Tenants: tenants,
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
		"a negative group id":   func(x *Tenant) { g := int64(-1); x.RunnerGroupID = &g },
		"the default group":     func(x *Tenant) { g := int64(1); x.RunnerGroupID = &g },
	}
	if err := tenantConfig(goodTenant()).validateTenants(); err != nil {
		t.Fatalf("a good tenant: %v", err)
	}
	for _, g := range []int64{0, 42} {
		x := goodTenant()
		x.RunnerGroupID = &g
		if err := tenantConfig(x).validateTenants(); err != nil {
			t.Errorf("runner_group_id %d: %v", g, err)
		}
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
	c = tenantConfig(goodTenant())
	c.Token, c.AppPrivateKey = "ghp_x", ""
	if err := c.validateTenants(); err == nil {
		t.Error("tenants with a client id but no key: accepted")
	}
}

// The cache has no boundary between tenants yet, and a tenant's tier must not
// change the catalog's own.
func TestTenantTiersHaveNoCacheOrIdleMachines(t *testing.T) {
	c := tenantConfig()
	got, _ := c.TenantTiers(goodTenant())
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

// A tenant's tier keeps the stopped pool its entry sets, never the
// catalog's; the entry may set one only on its own tiers, from 0 to the
// tier's max.
func TestTenantPool(t *testing.T) {
	c := tenantConfig()
	c.Tiers[0].Max = 10
	x := goodTenant()
	x.Limits = &TenantLimits{Pool: map[string]int{"cloud": 4}}
	got, _ := c.TenantTiers(x)
	if len(got) != 1 || got[0].EC2.Pool != 4 || got[0].EC2.PoolFor != nil || got[0].Min != 0 || got[0].Schedule != nil {
		t.Fatalf("a tenant's pooled tier: %+v, %+v", got[0], got[0].EC2)
	}
	if x.PoolSize("cloud") != 4 || x.PoolSize("other") != 0 || goodTenant().PoolSize("cloud") != 0 {
		t.Fatal("PoolSize")
	}
	if c.Tiers[0].EC2.Pool != 3 {
		t.Fatalf("the catalog's own pool changed: %d", c.Tiers[0].EC2.Pool)
	}
	if err := tenantConfig(x).validateTenants(); err != nil {
		t.Fatalf("a pool within the tier: %v", err)
	}
	for name, pool := range map[string]map[string]int{
		"a tier it is not given": {"micro": 1},
		"an unknown tier":        {"nope": 1},
		"negative":               {"cloud": -1},
		"above the tier's max":   {"cloud": 11},
	} {
		y := goodTenant()
		y.Limits = &TenantLimits{Pool: pool}
		cc := tenantConfig(y)
		cc.Tiers[0].Max = 10
		if err := cc.validateTenants(); err == nil || !strings.Contains(err.Error(), "limits.pool") {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	// A pool on a tier its vCPU limit refuses would never be served.
	big := goodTenant()
	big.Limits = &TenantLimits{Pool: map[string]int{"cloud": 1}}
	cb := tenantConfig(big)
	cb.Tiers[0].EC2.InstanceTypes = []string{"c8a.2xlarge"}
	if err := cb.validateTenants(); err == nil || !strings.Contains(err.Error(), "not served") {
		t.Errorf("a pool on a tier above the vCPU limit: %v, want refused", err)
	}
	if _, err := ParseTenants([]byte(`[{"id":"t1","url":"https://github.com/acme","installation_id":7,"tiers":["cloud"],"limits":{"pool":{"cloud":2}}}]`)); err != nil {
		t.Fatalf("an entry with a pool does not parse: %v", err)
	}
}

// A tenant's entry may keep hot runners on its own tiers, from 0 to the
// tier's max, and fewer in all than its runner limit: idle runners at the
// limit would hold every job.
func TestTenantHotRunners(t *testing.T) {
	c := tenantConfig()
	x := goodTenant()
	one := 1
	x.Limits = &TenantLimits{Min: map[string]int{"cloud": 1}}
	if err := tenantConfig(x).validateTenants(); err != nil {
		t.Fatalf("one hot runner under the default limit of 2: %v", err)
	}
	if x.MinSize("cloud") != 1 || x.MinSize("other") != 0 || goodTenant().MinSize("cloud") != 0 {
		t.Fatal("MinSize")
	}
	if got, _ := c.TenantTiers(x); got[0].Min != 0 {
		t.Fatalf("a tenant's tier starts with a floor of %d; its hot runners follow their target", got[0].Min)
	}
	for name, l := range map[string]*TenantLimits{
		"a tier it is not given": {Min: map[string]int{"micro": 1}},
		"negative":               {Min: map[string]int{"cloud": -1}},
		"above the tier's max":   {Min: map[string]int{"cloud": 11}, MaxRunners: &[]int{50}[0]},
		"at the runner limit":    {Min: map[string]int{"cloud": 2}},
		"at a set runner limit":  {Min: map[string]int{"cloud": 1}, MaxRunners: &one},
	} {
		y := goodTenant()
		y.Limits = l
		cc := tenantConfig(y)
		cc.Tiers[0].Max = 10
		if err := cc.validateTenants(); err == nil || !strings.Contains(err.Error(), "limits.min") {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	zero := 0
	z := goodTenant()
	z.Limits = &TenantLimits{Min: map[string]int{"cloud": 5}, MaxRunners: &zero}
	if err := tenantConfig(z).validateTenants(); err != nil {
		t.Errorf("hot runners with no runner limit: %v", err)
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
	if _, err := loadTenants(write(`[] [{"id":"t2"}]`)); err == nil {
		t.Fatal("a second list: accepted, and its tenants silently not served")
	}
	if _, err := loadTenants(write(`[] ]`)); err == nil {
		t.Fatal("a stray bracket: accepted")
	}
	if _, err := loadTenants(write("[]\n")); err != nil {
		t.Fatalf("a final newline: %v", err)
	}
	if _, err := loadTenants(write(`[] garbage`)); err == nil {
		t.Fatal("trailing garbage: accepted")
	}
	if _, err := loadTenants(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file: no error")
	}
}

// A KMS key stands in for the App's key, for tenants as for the install.
func TestTenantsWithAKMSKey(t *testing.T) {
	c := tenantConfig(goodTenant())
	c.AppPrivateKey, c.AppKMSKey = "", "alias/github-app"
	if err := c.validateTenants(); err != nil {
		t.Fatalf("a KMS key refused: %v", err)
	}
}

// An SSM parameter is read by the daemon, not at load: only its name is
// checked here. Its lists are checked as a file's are, each time one is read.
func TestTenantsInSSM(t *testing.T) {
	c := tenantConfig()
	c.TenantsPath = "ssm:/fr/control/tenants"
	if err := c.validateTenants(); err != nil {
		t.Fatalf("a parameter: %v", err)
	}
	for _, bad := range []string{"ssm:fr/control/tenants", "ssm:/fr/", "ssm:/fr//tenants", "ssm:"} {
		c.TenantsPath = bad
		if err := c.validateTenants(); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
	if err := c.CheckTenants([]Tenant{goodTenant()}); err != nil {
		t.Fatalf("a good list read: %v", err)
	}
	x := goodTenant()
	x.Tiers = []string{"nope"}
	if err := c.CheckTenants([]Tenant{x}); err == nil {
		t.Fatal("a list naming an unknown tier: accepted")
	}
	c.AppClientID = ""
	if err := c.CheckTenants(nil); err != nil {
		t.Fatalf("no tenants needs no App: %v", err)
	}
}

// db: is the job store's tenants table: it needs the store's URL, and names
// nothing more.
func TestTenantsInDB(t *testing.T) {
	c := tenantConfig()
	c.TenantsPath = "db:"
	if err := c.validateTenants(); err == nil || !strings.Contains(err.Error(), "FR_JOB_EVENTS_DSN") {
		t.Fatalf("db: with no job store: %v", err)
	}
	c.JobEventsDSNFile = "/etc/firerunner/job-events-dsn"
	if err := c.validateTenants(); err != nil {
		t.Fatalf("db: with a job store: %v", err)
	}
	c.TenantsPath = "db:tenants"
	if err := c.validateTenants(); err == nil {
		t.Fatal(`"db:tenants" accepted`)
	}
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://u@h/db")
	p, err := Parse([]string{"--url", "https://github.com/o", "--token", "t", "--tenants", "db:"})
	if err != nil || p.TenantsPath != "db:" || p.Tenants != nil {
		t.Fatalf("Parse: %v, tenants %q %v", err, p.TenantsPath, p.Tenants)
	}
}

// --entitlements db: is the job store's entitlements table: it needs the
// store's URL and names nothing more; the other stores check as before.
func TestEntitlementsInDB(t *testing.T) {
	c := &Config{Entitlements: "db:"}
	if err := c.checkEntitlements(); err == nil || !strings.Contains(err.Error(), "FR_JOB_EVENTS_DSN") {
		t.Fatalf("db: with no job store: %v", err)
	}
	c.JobEventsDSN = "postgres://u@h/db"
	if err := c.checkEntitlements(); err != nil {
		t.Fatalf("db: with a job store: %v", err)
	}
	for _, bad := range []string{"db:entitlements", "relative.json", "ssm:relative"} {
		c.Entitlements = bad
		if err := c.checkEntitlements(); err == nil || !strings.Contains(err.Error(), "--entitlements") {
			t.Errorf("%q: %v, want refused", bad, err)
		}
	}
	for _, good := range []string{"", "/etc/firerunner/entitlements.json", "ssm:/fr/control/entitlements"} {
		c.Entitlements = good
		if err := c.checkEntitlements(); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://u@h/db")
	if _, err := Parse([]string{"--url", "https://github.com/o", "--token", "t", "--entitlements", "db:"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// --tenant-leases keeps its leases beside the tenants table, and hands a
// tenant's runners on by --owner: it needs both.
func TestTenantLeasesNeedDBAndOwner(t *testing.T) {
	c := tenantConfig()
	c.JobEventsDSNFile = "/etc/firerunner/job-events-dsn"
	c.TenantLeases = true
	c.Owner, c.TenantOwner = "vm-1", "fr"
	c.TenantsPath = "ssm:/fr/control/tenants"
	if err := c.validateTenants(); err == nil || !strings.Contains(err.Error(), "--tenants db:") {
		t.Fatalf("leases on a parameter: %v", err)
	}
	c.TenantsPath = "db:"
	if !c.UsesEC2() {
		t.Fatal("the fixture has no EC2 tier")
	}
	if err := c.validateTenants(); err == nil || !strings.Contains(err.Error(), "--ec2-pool-key-file") {
		t.Fatalf("leases with EC2 and no pool key: %v", err)
	}
	c.EC2PoolKeyFile = "/etc/firerunner/pool-key"
	if err := c.validateTenants(); err != nil {
		t.Fatalf("leases on db: with owners and a pool key: %v", err)
	}
	c.Owner = ""
	if err := c.validateTenants(); err == nil || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("leases with no owner: %v", err)
	}
	c.Owner, c.TenantOwner = "vm-1", ""
	if err := c.validateTenants(); err == nil || !strings.Contains(err.Error(), "--tenant-owner") {
		t.Fatalf("leases with no tenant owner: %v", err)
	}
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://u@h/db")
	t.Setenv("FR_TENANT_LEASES", "true")
	p, err := Parse([]string{"--url", "https://github.com/o", "--token", "t", "--tenants", "db:", "--owner", "vm-1", "--tenant-owner", "fr"})
	if err != nil || !p.TenantLeases || p.TenantOwner != "fr" {
		t.Fatalf("Parse: %v, leases %v, tenant owner %q", err, p.TenantLeases, p.TenantOwner)
	}
	for _, bad := range []string{"fr/x", " fr", "aws:fr", strings.Repeat("f", 65)} {
		p.TenantOwner = bad
		if err := p.validateTenants(); err == nil || !strings.Contains(err.Error(), "--tenant-owner") {
			t.Fatalf("tenant owner %q: %v", bad, err)
		}
	}
}

// A parameter is not a file: Parse reads nothing for it.
func TestParseLeavesAParameterToTheDaemon(t *testing.T) {
	c, err := Parse([]string{"--url", "https://github.com/o", "--token", "t", "--tenants", "ssm:/fr/control/tenants"})
	if err != nil {
		t.Fatal(err)
	}
	if c.TenantsPath != "ssm:/fr/control/tenants" || c.Tenants != nil {
		t.Fatalf("tenants %q %v", c.TenantsPath, c.Tenants)
	}
}
