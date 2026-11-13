package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsgd "github.com/aws/aws-sdk-go-v2/service/guardduty"
	"github.com/aws/aws-sdk-go-v2/service/guardduty/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

// gdFake is a GuardDuty with one detector holding findings; it lists them
// all (internal/guardduty's own fake checks the criteria).
type gdFake struct {
	mu       sync.Mutex
	findings []types.Finding
}

func (g *gdFake) add(id string, sev float64, updated time.Time, owner string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.findings = append(g.findings, types.Finding{
		Id: aws.String(id), Type: aws.String("CryptoCurrency:EC2/BitcoinTool.B!DNS"), Severity: aws.Float64(sev),
		UpdatedAt: aws.String(updated.UTC().Format(time.RFC3339Nano)),
		Resource: &types.Resource{ResourceType: aws.String("Instance"), InstanceDetails: &types.InstanceDetails{
			InstanceId: aws.String("i-" + id),
			Tags: []types.Tag{
				{Key: aws.String("firerunner:owner"), Value: aws.String(owner)},
				{Key: aws.String("firerunner:tier"), Value: aws.String("small")},
			},
		}},
		Service: &types.Service{Archived: aws.Bool(false), Count: aws.Int32(1)},
	})
}

func (g *gdFake) ListDetectors(context.Context, *awsgd.ListDetectorsInput, ...func(*awsgd.Options)) (*awsgd.ListDetectorsOutput, error) {
	return &awsgd.ListDetectorsOutput{DetectorIds: []string{"d"}}, nil
}

func (g *gdFake) ListFindings(context.Context, *awsgd.ListFindingsInput, ...func(*awsgd.Options)) (*awsgd.ListFindingsOutput, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := &awsgd.ListFindingsOutput{}
	for _, f := range g.findings {
		out.FindingIds = append(out.FindingIds, aws.ToString(f.Id))
	}
	return out, nil
}

func (g *gdFake) GetFindings(context.Context, *awsgd.GetFindingsInput, ...func(*awsgd.Options)) (*awsgd.GetFindingsOutput, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return &awsgd.GetFindingsOutput{Findings: append([]types.Finding(nil), g.findings...)}, nil
}

type eventLog struct {
	mu     sync.Mutex
	events []jobevent.Event
}

func (l *eventLog) Record(e jobevent.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) all() []jobevent.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]jobevent.Event(nil), l.events...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A finding on a served tenant's machine flags it in the health file and
// the job events, and never suspends or holds it: its launches go on. A
// finding on another install's machine flags nothing. Once an operator clears
// the flag, the next read drops it.
func TestWatchFindingsFlagsWithoutSuspending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	events := &eventLog{}
	s := &shard{log: slog.New(slog.DiscardHandler), events: events}
	live := &liveTenants{}
	acme := &servedTenant{t: tenant{ID: "acme"}, bound: scheduler.NewTenant("acme", 0)}
	live.add(acme)
	cfg := &config.Config{TenantState: path, GuardDutyMinSeverity: 4, Tenants: []config.Tenant{{ID: "acme"}}}
	if err := s.watchStates(ctx, cfg, live, s.log); err != nil {
		t.Fatal(err)
	}
	gd := &gdFake{}
	gd.add("f1", 8, time.Now().Add(-time.Minute), "inst/acme")
	gd.add("f2", 8, time.Now().Add(-time.Minute), "other-install/acme")
	flags, err := tenantstate.OpenFlags(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.watchFindings(ctx, cfg, "inst", gd, flags, nil, s.log)
	waitFor(t, "the first read of findings", func() bool { return !s.findings.Status().LastPoll.IsZero() })

	states, _ := s.states.States()
	h := live.tenants(states, s.findings.Flags())
	if len(h) != 1 || h[0].Flag == nil || h[0].Flag.Type != "CryptoCurrency:EC2/BitcoinTool.B!DNS" || h[0].Flag.Instance != "i-f1" || h[0].Flag.Count != 1 {
		t.Fatalf("health = %+v", h)
	}
	if h[0].State != "" {
		t.Fatalf("a flag changed the tenant's state to %q", h[0].State)
	}
	if susp, why := acme.bound.State(); susp || why != "" {
		t.Fatalf("a flag suspended or held the tenant: %v %q", susp, why)
	}
	if st, err := (&tenantstate.FileStore{Path: path}).Load(ctx); err != nil || len(st) != 0 {
		t.Fatalf("a flag wrote a tenant state: %v, %v", st, err)
	}
	ev := events.all()
	if len(ev) != 1 || ev[0].Kind != jobevent.Flagged || ev[0].Instance != "i-f1" || ev[0].Tier != "small" || ev[0].Tenant != "acme" ||
		!strings.Contains(ev[0].Reason, "tenant acme flagged for review") || ev[0].Runner != "" {
		t.Fatalf("events = %+v", ev)
	}
	if g := healthGuardDuty(s.findings.Status()); g.Detectors != 1 || g.MinSeverity != 4 || !strings.HasSuffix(g.Flags, "tenant-state-flags.json") {
		t.Fatalf("health guardduty = %+v", g)
	}

	if code, out, errOut := runTenant(t, "clear-flag", "acme", "--state", path); code != 0 {
		t.Fatalf("clear-flag: %d %s%s", code, out, errOut)
	}
	if err := s.findings.Poll(ctx); err != nil { // the next tick's read
		t.Fatal(err)
	}
	if h := live.tenants(states, s.findings.Flags()); h != nil && h[0].Flag != nil {
		t.Fatalf("after clear-flag: %+v", h[0].Flag)
	}
	if len(events.all()) != 1 {
		t.Fatalf("a cleared finding flagged again: %+v", events.all())
	}
}

// list shows flags beside suspensions; clear-flag removes one, and leaves
// the suspensions alone.
func TestTenantFlagCommands(t *testing.T) {
	t.Setenv("FR_TENANT_STATE", "")
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	flags, err := tenantstate.OpenFlags(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 11, 3, 7, 23, 0, time.UTC)
	if err := flags.PutFlag(context.Background(), "acme", tenantstate.Flag{Type: "CryptoCurrency:EC2/BitcoinTool.B!DNS", Severity: 8, Count: 1, First: at, Last: at, Finding: "f1", Instance: "i-0aaa"}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runTenant(t, "suspend", "beta", "--reason", "abuse", "--state", path); code != 0 {
		t.Fatal("suspend failed")
	}
	code, out, errOut := runTenant(t, "list", "--state", path)
	if code != 0 || !strings.Contains(out, "acme\tflagged for review: CryptoCurrency:EC2/BitcoinTool.B!DNS, severity 8, 1 finding(s) or update(s) from 2026-11-11T03:07:23Z") ||
		!strings.Contains(out, "beta\tsuspended since") {
		t.Fatalf("list: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = runTenant(t, "clear-flag", "acme", "--state", path, "--json")
	var r tenantReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || r.ID != "acme" || r.State != "cleared" || !strings.HasSuffix(r.Flags, "tenant-state-flags.json") {
		t.Fatalf("clear-flag: exit %d, %+v, %v\n%s", code, r, err, out)
	}
	code, out, _ = runTenant(t, "list", "--state", path, "--json")
	r = tenantReport{}
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || len(r.Flagged) != 0 || len(r.Suspended) != 1 {
		t.Fatalf("list after clear-flag: exit %d, %+v, %v", code, r, err)
	}
	if code, out, _ := runTenant(t, "clear-flag", "acme", "--state", path); code != 0 || !strings.Contains(out, "acme is not flagged") {
		t.Fatalf("a second clear-flag: exit %d\n%s", code, out)
	}

	// Flags that cannot be read fail list, after it prints the
	// suspensions; a bad severity setting never blocks a suspension.
	if err := os.WriteFile(flags.String(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runTenant(t, "list", "--state", path)
	if code != 1 || !strings.Contains(out, "beta\tsuspended since") || !strings.Contains(errOut, "tenant-state-flags.json") {
		t.Fatalf("list with unreadable flags: exit %d\n%s%s", code, out, errOut)
	}
	t.Setenv("FR_GUARDDUTY_MIN_SEVERITY", "high")
	if code, out, errOut := runTenant(t, "suspend", "gamma", "--reason", "abuse", "--state", path); code != 0 {
		t.Fatalf("suspend with a bad severity setting: exit %d\n%s%s", code, out, errOut)
	}
}

// Findings are read with --guardduty and an EC2 tier; a Firecracker-only
// install never calls GuardDuty.
func TestReadsFindings(t *testing.T) {
	ec2Tier := config.Tier{Name: "cloud", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}
	fcTier := config.Tier{Name: "local"}
	for _, c := range []struct {
		on    bool
		tiers []config.Tier
		want  bool
	}{
		{true, []config.Tier{ec2Tier}, true},
		{true, []config.Tier{fcTier, ec2Tier}, true},
		{true, []config.Tier{fcTier}, false},
		{false, []config.Tier{ec2Tier}, false},
	} {
		if got := readsFindings(&config.Config{GuardDuty: c.on, Tiers: c.tiers}); got != c.want {
			t.Errorf("guardduty %v, %d tiers: %v, want %v", c.on, len(c.tiers), got, c.want)
		}
	}
}

// findings lists the findings on the owner's instances and the tenant each
// names, leaving out other owners' and those older than --since.
func TestTenantFindings(t *testing.T) {
	t.Setenv("FR_OWNER", "inst")
	t.Setenv("FR_GUARDDUTY_MIN_SEVERITY", "")
	gd := &gdFake{}
	gd.add("f-acme", 8, time.Now().Add(-time.Hour), "inst/acme")
	gd.add("f-install", 5, time.Now().Add(-2*time.Hour), "inst")
	gd.add("f-other", 8, time.Now().Add(-time.Hour), "someone-else")
	gd.add("f-old", 8, time.Now().Add(-48*time.Hour), "inst/acme")
	gd.add("f-low", 2, time.Now().Add(-time.Hour), "inst/acme")
	code, out, errOut := runTenantOn(t, gd, "findings", "--json")
	var r tenantReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 {
		t.Fatalf("findings: exit %d, %v\n%s%s", code, err, out, errOut)
	}
	if len(r.Findings) != 2 || r.Findings[0].ID != "f-install" || r.Findings[0].Tenant != tenantstate.Install ||
		r.Findings[1].ID != "f-acme" || r.Findings[1].Tenant != "acme" || r.Findings[1].Tier != "small" {
		t.Fatalf("findings = %+v", r.Findings)
	}
	code, out, _ = runTenantOn(t, gd, "findings", "--since", "90m", "--min-severity", "7")
	if code != 0 || !strings.Contains(out, "acme\tCryptoCurrency:EC2/BitcoinTool.B!DNS\tseverity 8") || strings.Contains(out, "f-install") {
		t.Fatalf("findings --since 90m --min-severity 7: exit %d\n%s", code, out)
	}
	code, out, _ = runTenantOn(t, gd, "findings", "--owner", "nobody")
	if code != 0 || !strings.Contains(out, "no finding of severity 4 and above on nobody's instances") {
		t.Fatalf("findings for another owner: exit %d\n%s", code, out)
	}
}
