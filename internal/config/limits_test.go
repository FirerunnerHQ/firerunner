package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInstanceVCPU(t *testing.T) {
	for itype, want := range map[string]int{
		"t3.nano": 2, "m6g.medium": 2, "c8a.large": 2, "c8a.xlarge": 4, "c7a.2xlarge": 8,
		"m8azn.12xlarge": 48, "u7i-12tb.224xlarge": 896,
	} {
		if got, ok := InstanceVCPU(itype); !ok || got != want {
			t.Errorf("%s: %d, %v; want %d", itype, got, ok, want)
		}
	}
	for _, itype := range []string{"c8a.metal", "mac2.metal", "c8a", "", "c8a.0xlarge", "c8a.bigxlarge", "c8a.huge"} {
		if n, ok := InstanceVCPU(itype); ok {
			t.Errorf("%s: read as %d vCPU", itype, n)
		}
	}
}

// An EC2 tier is as large as its largest fallback type: a launch may land on
// any of them.
func TestTierVCPU(t *testing.T) {
	ec2 := func(types ...string) Tier {
		return Tier{Name: "e", Provisioner: ProvisionerEC2, EC2: &EC2Tier{InstanceTypes: types}}
	}
	if n, ok := TierVCPU(ec2("c8a.large", "c7a.xlarge")); !ok || n != 4 {
		t.Fatalf("= %d, %v", n, ok)
	}
	if _, ok := TierVCPU(ec2("c8a.large", "c8a.metal")); ok {
		t.Fatal("a size that cannot be read was not reported")
	}
	if n, ok := TierVCPU(Tier{Name: "fc", VCPU: 3}); !ok || n != 3 {
		t.Fatalf("firecracker = %d, %v", n, ok)
	}
}

// A tenant that sets no limits gets the defaults; one that sets some keeps the
// defaults for the rest; 0 lifts a limit.
func TestEffectiveLimits(t *testing.T) {
	def := Limits{Runners: DefaultTenantRunners, VCPU: DefaultTenantVCPU, JobDuration: DefaultTenantJobDuration}
	if got := (Tenant{}).EffectiveLimits(); got != def {
		t.Fatalf("no limits: %+v", got)
	}
	var x Tenant
	if err := json.Unmarshal([]byte(`{"limits":{"max_runners":0,"max_job_duration":"1h"}}`), &x); err != nil {
		t.Fatal(err)
	}
	want := Limits{Runners: 0, VCPU: DefaultTenantVCPU, JobDuration: time.Hour}
	if got := x.EffectiveLimits(); got != want {
		t.Fatalf("some limits: %+v, want %+v", got, want)
	}
}

func TestTenantTiersWithinVCPU(t *testing.T) {
	c := tenantConfig()
	c.Tiers = append(c.Tiers,
		Tier{Name: "small", Provisioner: ProvisionerEC2, EC2: &EC2Tier{InstanceTypes: []string{"c8a.large"}}},
		Tier{Name: "big", Provisioner: ProvisionerEC2, EC2: &EC2Tier{InstanceTypes: []string{"c8a.large", "c8a.xlarge"}}},
		Tier{Name: "metal", Provisioner: ProvisionerEC2, EC2: &EC2Tier{InstanceTypes: []string{"c8a.metal"}}},
	)
	x := goodTenant()
	x.Tiers = []string{"small", "big", "metal"}
	tiers, refused := c.TenantTiers(x)
	if len(tiers) != 1 || tiers[0].Name != "small" {
		t.Fatalf("served %+v", tiers)
	}
	if len(refused) != 2 || !strings.Contains(refused[0], `"big": 4 vCPU is above the limit of 2`) || !strings.Contains(refused[1], `"metal"`) {
		t.Fatalf("refused %q", refused)
	}
	none := 0
	x.Limits = &TenantLimits{MaxVCPU: &none}
	if tiers, refused := c.TenantTiers(x); len(tiers) != 3 || len(refused) != 0 {
		t.Fatalf("no vCPU limit: served %d, refused %q", len(tiers), refused)
	}
}

// The install is not limited unless its flags say so.
func TestInstallTiers(t *testing.T) {
	c := &Config{Tiers: []Tier{{Name: "fc", VCPU: 4}, {Name: "fc2", VCPU: 2}}}
	if tiers, refused := c.InstallTiers(); len(tiers) != 2 || refused != nil {
		t.Fatalf("no limit: %d, %q", len(tiers), refused)
	}
	c.Limits.VCPU = 2
	if tiers, refused := c.InstallTiers(); len(tiers) != 1 || tiers[0].Name != "fc2" || len(refused) != 1 {
		t.Fatalf("limit 2: %+v, %q", tiers, refused)
	}
}

func TestLimitsValidate(t *testing.T) {
	for _, l := range []Limits{{}, {Runners: 1, VCPU: 2, JobDuration: time.Minute}} {
		if err := l.validate("", flagLimitNames); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	for _, l := range []Limits{{Runners: -1}, {VCPU: -1}, {JobDuration: -time.Minute}, {JobDuration: 59 * time.Second}} {
		if err := l.validate("", flagLimitNames); err == nil {
			t.Errorf("%+v: accepted", l)
		}
	}
}

func TestLimitsJSONRoundTrip(t *testing.T) {
	in := Limits{Runners: 2, VCPU: 2, JobDuration: 30 * time.Minute}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"max_runners":2,"max_vcpu":2,"max_job_duration":"30m0s"}` {
		t.Fatalf("json = %s", b)
	}
	var out Limits
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
}
