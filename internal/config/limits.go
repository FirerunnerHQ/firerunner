package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A tenant whose catalog entry sets no limits gets these: a new tenant runs
// little, on small machines, for a bounded time, until an operator raises them.
// Hosted compute runs arbitrary code on the operator's account (#169).
const (
	DefaultTenantRunners     = 2
	DefaultTenantVCPU        = 2
	DefaultTenantJobDuration = 30 * time.Minute
)

// minJobDuration is the shortest job limit: anything shorter ends jobs while
// they set up.
const minJobDuration = time.Minute

// Limits bound what one tenant runs. Zero means no limit.
type Limits struct {
	// Runners is the most runners the tenant has at once, across its tiers.
	Runners int `json:"max_runners"`
	// VCPU is the most vCPUs a tier served to the tenant may have. A larger
	// tier is not served to it.
	VCPU int `json:"max_vcpu"`
	// JobDuration ends a runner whose job has run this long.
	JobDuration time.Duration `json:"-"`
}

// MarshalJSON writes JobDuration as a duration string, as the catalog does.
func (l Limits) MarshalJSON() ([]byte, error) {
	type plain Limits
	return json.Marshal(struct {
		plain
		JobDuration string `json:"max_job_duration"`
	}{plain(l), l.JobDuration.String()})
}

// UnmarshalJSON reads what MarshalJSON writes.
func (l *Limits) UnmarshalJSON(b []byte) error {
	type plain Limits
	var v struct {
		plain
		JobDuration Duration `json:"max_job_duration"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*l = Limits(v.plain)
	l.JobDuration = time.Duration(v.JobDuration)
	return nil
}

// None reports whether l limits nothing.
func (l Limits) None() bool { return l == Limits{} }

// limitNames are what a configuration calls each limit, for its errors.
type limitNames struct{ runners, vcpu, job string }

var (
	catalogLimitNames = limitNames{"limits.max_runners", "limits.max_vcpu", "limits.max_job_duration"}
	flagLimitNames    = limitNames{"--limit-runners", "--limit-vcpu", "--limit-job-duration"}
)

func (l Limits) validate(prefix string, n limitNames) error {
	switch {
	case l.Runners < 0:
		return fmt.Errorf("%s%s must be >= 0 (0 = no limit)", prefix, n.runners)
	case l.VCPU < 0:
		return fmt.Errorf("%s%s must be >= 0 (0 = no limit)", prefix, n.vcpu)
	case l.JobDuration < 0:
		return fmt.Errorf("%s%s must be >= 0 (0 = no limit)", prefix, n.job)
	case l.JobDuration != 0 && l.JobDuration < minJobDuration:
		return fmt.Errorf("%s%s %s is too short: use 0 for no limit, or at least %s", prefix, n.job, l.JobDuration, minJobDuration)
	}
	return nil
}

// String describes l for logs and status.
func (l Limits) String() string {
	part := func(n int, unit string) string {
		if n == 0 {
			return "no " + unit + " limit"
		}
		return fmt.Sprintf("%d %s", n, unit)
	}
	job := "no job limit"
	if l.JobDuration > 0 {
		job = l.JobDuration.String() + " per job"
	}
	return part(l.Runners, "runner(s)") + ", " + part(l.VCPU, "vCPU per tier") + ", " + job
}

// TenantLimits is a catalog entry's "limits" as written. A field left out
// keeps its default; 0 lifts that limit.
type TenantLimits struct {
	MaxRunners     *int      `json:"max_runners,omitempty"`
	MaxVCPU        *int      `json:"max_vcpu,omitempty"`
	MaxJobDuration *Duration `json:"max_job_duration,omitempty"`
}

// Duration is a time.Duration written as a string, e.g. "30m"; "0" is none.
type Duration time.Duration

// UnmarshalJSON reads a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`a duration is a string such as "30m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// EffectiveLimits are the tenant's limits: its own where it sets them, the
// defaults elsewhere.
func (t Tenant) EffectiveLimits() Limits {
	l := Limits{Runners: DefaultTenantRunners, VCPU: DefaultTenantVCPU, JobDuration: DefaultTenantJobDuration}
	if t.Limits == nil {
		return l
	}
	if t.Limits.MaxRunners != nil {
		l.Runners = *t.Limits.MaxRunners
	}
	if t.Limits.MaxVCPU != nil {
		l.VCPU = *t.Limits.MaxVCPU
	}
	if t.Limits.MaxJobDuration != nil {
		l.JobDuration = time.Duration(*t.Limits.MaxJobDuration)
	}
	return l
}

// InstanceVCPU returns how many vCPUs an EC2 instance type has, read from its
// size: large has 2, xlarge 4 and Nxlarge 4N. The sizes below large count as 2,
// the most any of them has. ok is false for a size it cannot read, such as
// metal.
func InstanceVCPU(itype string) (int, bool) {
	_, size, found := strings.Cut(itype, ".")
	if !found {
		return 0, false
	}
	switch size {
	case "nano", "micro", "small", "medium", "large":
		return 2, true
	case "xlarge":
		return 4, true
	}
	n, err := strconv.Atoi(strings.TrimSuffix(size, "xlarge"))
	if err != nil || !strings.HasSuffix(size, "xlarge") || n < 1 {
		return 0, false
	}
	return 4 * n, true
}

// TierVCPU returns the most vCPUs a runner of the tier can have: a Firecracker
// tier's vcpu, or an EC2 tier's largest instance type. ok is false when an
// instance type's size cannot be read.
func TierVCPU(t Tier) (int, bool) {
	if !t.IsEC2() {
		return t.VCPU, true
	}
	most := 0
	for _, it := range t.EC2.InstanceTypes {
		n, ok := InstanceVCPU(it)
		if !ok {
			return 0, false
		}
		most = max(most, n)
	}
	return most, true
}

// withinVCPU splits tiers into those a vCPU limit allows and, for the others,
// why not. A limit of 0 allows every tier.
func withinVCPU(tiers []Tier, limit int) (allowed []Tier, refused []string) {
	for _, t := range tiers {
		n, ok := TierVCPU(t)
		switch {
		case limit == 0:
		case !ok:
			refused = append(refused, fmt.Sprintf("tier %q: its instance types' vCPUs cannot be read from their names; a limit of %d vCPU does not serve it", t.Name, limit))
			continue
		case n > limit:
			refused = append(refused, fmt.Sprintf("tier %q: %d vCPU is above the limit of %d", t.Name, n, limit))
			continue
		}
		allowed = append(allowed, t)
	}
	return allowed, refused
}

// InstallTiers returns the tiers the install serves itself: every tier within
// its own vCPU limit (--limit-vcpu), and why it leaves out the others.
func (c *Config) InstallTiers() ([]Tier, []string) {
	return withinVCPU(c.EffectiveTiers(), c.Limits.VCPU)
}
