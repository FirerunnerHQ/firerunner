package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/guardduty"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

func runTenant(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runTenantOn(t, nil, args...)
}

// runTenantOn runs the tenant subcommand with file stores and gd as
// GuardDuty.
func runTenantOn(t *testing.T, gd guardduty.API, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := tenantCmd(args, &out, &errOut, tenantOpeners{
		states: func(spec string) (tenantstate.Store, error) { return tenantstate.Open(spec, nil) },
		flags:  func(spec string) (tenantstate.FlagStore, error) { return tenantstate.OpenFlags(spec, nil) },
		guardDuty: func() (guardduty.API, string, error) {
			if gd == nil {
				t.Fatal("GuardDuty opened")
			}
			return gd, "", nil
		},
	})
	return code, out.String(), errOut.String()
}

// Suspend, list and resume round-trip through the store the daemon reads; a
// second store over it (a restarted daemon) sees the suspension.
func TestTenantCommand(t *testing.T) {
	t.Setenv("FR_TENANT_STATE", "")
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	code, out, errOut := runTenant(t, "suspend", "acme", "--reason", "abuse report", "--state", path)
	if code != 0 || !strings.Contains(out, "suspended acme") || !strings.Contains(out, "15s") {
		t.Fatalf("suspend: exit %d\n%s%s", code, out, errOut)
	}
	st, err := (&tenantstate.FileStore{Path: path}).Load(context.Background())
	if err != nil || st["acme"].Reason != "abuse report" || st["acme"].Since.IsZero() {
		t.Fatalf("store holds %+v, %v", st, err)
	}

	t.Setenv("FR_TENANT_STATE", path) // the daemon's own setting is the default
	code, out, _ = runTenant(t, "list", "--json")
	var r tenantReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || len(r.Suspended) != 1 || r.Suspended[0].ID != "acme" {
		t.Fatalf("list: exit %d, %+v, %v\n%s", code, r, err, out)
	}
	if code, out, _ := runTenant(t, "suspend", "_install", "--reason=test"); code != 0 || !strings.Contains(out, "_install") {
		t.Fatalf("suspend the install: exit %d\n%s", code, out)
	}
	code, out, _ = runTenant(t, "resume", "acme", "--json")
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || r.State != "active" || r.ID != "acme" {
		t.Fatalf("resume: exit %d, %+v\n%s", code, r, out)
	}
	if code, out, _ := runTenant(t, "list"); code != 0 || strings.Contains(out, "acme") || !strings.Contains(out, "_install") {
		t.Fatalf("list after resume: exit %d\n%s", code, out)
	}
}

// Usage errors exit 2 and say what is wrong; nothing is written.
func TestTenantCommandUsage(t *testing.T) {
	t.Setenv("FR_TENANT_STATE", "")
	t.Setenv("FR_OWNER", "")
	t.Setenv("FR_GUARDDUTY_MIN_SEVERITY", "")
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "name an action"},
		{[]string{"pause", "acme"}, "unknown action"},
		{[]string{"suspend", "acme", "--state", path}, "--reason is required"},
		{[]string{"suspend", "acme", "--reason", " ", "--state", path}, "--reason is required"},
		{[]string{"suspend", "--reason", "x", "--state", path}, "takes one tenant id"},
		{[]string{"suspend", "a", "b", "--reason", "x", "--state", path}, "takes one tenant id"},
		{[]string{"suspend", "Acme", "--reason", "x", "--state", path}, "is not a tenant id"},
		{[]string{"resume", "acme", "--reason", "x", "--state", path}, "--reason is for suspend"},
		{[]string{"suspend", "acme", "--reason", "x"}, "no tenant state store"},
		{[]string{"suspend", "acme", "--reason", "x", "--state", "relative.json"}, "absolute"},
		{[]string{"list", "acme", "--state", path}, "takes no id"},
		{[]string{"list", "--state"}, "--state needs a value"},
		{[]string{"list", "--force", "--state", path}, "unknown flag"},
		{[]string{"list", "--json=false", "--state", path}, "--json takes no value"},
		{[]string{"resume", "acme", "--reason=", "--state", path}, "--reason is for suspend"},
		{[]string{"clear-flag", "--state", path}, "takes one tenant id"},
		{[]string{"clear-flag", "Acme", "--state", path}, "is not a tenant id"},
		{[]string{"clear-flag", "acme"}, "no tenant state store"},
		{[]string{"list", "--owner", "fr", "--state", path}, "--owner is for findings"},
		{[]string{"findings", "--owner", "fr", "--state", path}, "--state is for suspend, resume, clear-flag, list"},
		{[]string{"findings", "acme", "--owner", "fr"}, "takes no id"},
		{[]string{"findings"}, "no owner"},
		{[]string{"findings", "--owner", "fr", "--since", "a day"}, "give a duration"},
		{[]string{"findings", "--owner", "fr", "--since", "-1h"}, "give a duration"},
		{[]string{"findings", "--owner", "fr", "--min-severity", "0"}, "from 1 to 10"},
		{[]string{"findings", "--owner", "fr", "--min-severity", "high"}, "not a number"},
	} {
		code, out, errOut := runTenant(t, c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) || out != "" {
			t.Errorf("%q: exit %d, want 2 and %q\nstdout %s\nstderr %s", c.args, code, c.want, out, errOut)
		}
	}
	if code, out, _ := runTenant(t, "--help"); code != 0 || !strings.Contains(out, "tenant suspend ID") {
		t.Fatalf("--help: exit %d\n%s", code, out)
	}
	if code, out, _ := runTenant(t, "suspend", "-h"); code != 0 || !strings.Contains(out, "_install") {
		t.Fatalf("suspend -h: exit %d\n%s", code, out)
	}
	if st, _ := (&tenantstate.FileStore{Path: path}).Load(context.Background()); len(st) != 0 {
		t.Fatalf("a usage error wrote %v", st)
	}
}

// statesFor is a shard reading states from a file store, read once.
func statesFor(t *testing.T, st tenantstate.States) (*shard, *tenantstate.FileStore) {
	t.Helper()
	store := &tenantstate.FileStore{Path: filepath.Join(t.TempDir(), "tenant-state.json")}
	for id, r := range st {
		if err := store.Put(context.Background(), id, r); err != nil {
			t.Fatal(err)
		}
	}
	w := &tenantstate.Watcher{Store: store}
	if err := w.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &shard{log: slog.New(slog.DiscardHandler), states: w}, store
}

// A tenant is suspended by its record, at start (a restart keeps it) and on a
// change; resumed when the record goes; the install by _install.
func TestApplyState(t *testing.T) {
	s, store := statesFor(t, tenantstate.States{"acme": {Reason: "abuse"}, tenantstate.Install: {Reason: "test"}})
	acme, other, install := scheduler.NewTenant("acme", 0), scheduler.NewTenant("other", 0), scheduler.NewTenant("", 0)
	s.applyState("acme", acme, s.log)
	s.applyState("other", other, s.log)
	s.applyState("", install, s.log)
	if susp, why := acme.State(); !susp || !strings.Contains(why, "abuse") {
		t.Fatalf("acme: %v %q", susp, why)
	}
	if susp, why := other.State(); susp || why != "" {
		t.Fatalf("other: %v %q", susp, why)
	}
	if susp, _ := install.State(); !susp {
		t.Fatal("the install's record did not suspend it")
	}
	if got := s.suspension("acme")(); got != "abuse" {
		t.Fatalf("the reaper is told %q", got)
	}

	if err := store.Delete(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if err := s.states.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.applyState("acme", acme, s.log)
	if susp, why := acme.State(); susp || why != "" {
		t.Fatalf("acme after resume: %v %q", susp, why)
	}
	if got := s.suspension("acme")(); got != "" {
		t.Fatalf("the reaper is told %q after the resume", got)
	}
}

// Until a read succeeds, the catalog's tenants are held, and the install is
// not: an unreadable store never serves a tenant that may be suspended.
func TestApplyStateHoldsUntilRead(t *testing.T) {
	w := &tenantstate.Watcher{Store: brokenStore{}}
	_ = w.Read(context.Background())
	s := &shard{log: slog.New(slog.DiscardHandler), states: w}
	acme, install := scheduler.NewTenant("acme", 0), scheduler.NewTenant("", 0)
	s.applyState("acme", acme, s.log)
	s.applyState("", install, s.log)
	if susp, why := acme.State(); susp || !strings.Contains(why, "has not been read yet") {
		t.Fatalf("acme: %v %q", susp, why)
	}
	if _, why := install.State(); why != "" {
		t.Fatalf("the install was held: %q", why)
	}
}

// The daemon applies a suspension written while it runs to the tenant served,
// at the next read: the watcher's tick, every tenantstate.DefaultInterval.
// The tenant's runners are stopped then (see scheduler's Suspend tests).
func TestWatchStatesAppliesASuspensionAtTheNextRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	s := &shard{log: slog.New(slog.DiscardHandler)}
	live := &liveTenants{}
	acme := &servedTenant{t: tenant{ID: "acme"}, bound: scheduler.NewTenant("acme", 0)}
	live.add(acme)
	if err := s.watchStates(ctx, &config.Config{TenantState: path}, live, s.log); err != nil {
		t.Fatal(err)
	}
	w := s.states
	if susp, _ := acme.bound.State(); susp {
		t.Fatal("suspended before any suspension")
	}
	if code, out, errOut := runTenant(t, "suspend", "acme", "--reason", "abuse", "--state", path); code != 0 {
		t.Fatalf("suspend: %d %s%s", code, out, errOut)
	}
	if err := w.Read(ctx); err != nil { // the next tick's read
		t.Fatal(err)
	}
	if susp, why := acme.bound.State(); !susp || !strings.Contains(why, "abuse") {
		t.Fatalf("after the read: %v %q", susp, why)
	}
	h := live.tenants(func() tenantstate.States { st, _ := w.States(); return st }(), nil)
	if len(h) != 1 || h[0].State != "suspended" || h[0].Reason != "abuse" || h[0].Since.IsZero() {
		t.Fatalf("health = %+v", h)
	}
	if code, _, _ := runTenant(t, "resume", "acme", "--state", path); code != 0 {
		t.Fatal("resume failed")
	}
	if err := w.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if susp, why := acme.bound.State(); susp || why != "" {
		t.Fatalf("after the resume's read: %v %q", susp, why)
	}
}

// A tenant started after its suspension, as after a restart, starts
// suspended, and its reaper ends its orphans busy or not, and by its job
// limit.
func TestTenantStartsInItsState(t *testing.T) {
	s, _ := statesFor(t, tenantstate.States{"acme": {Reason: "abuse"}})
	s.cfg = &config.Config{}
	acme := tenant{ID: "acme", Owner: "o/acme", Limits: config.Limits{Runners: 2, JobDuration: 30 * time.Minute}}
	if susp, why := s.newBound(acme, s.log).State(); !susp || !strings.Contains(why, "abuse") {
		t.Fatalf("started %v %q", susp, why)
	}
	ro := s.tenantReaperOptions(acme, nil, s.log)
	if ro.Suspended == nil || ro.Suspended() != "abuse" || ro.MaxLifetime != 45*time.Minute || ro.Owner != "o/acme" {
		t.Fatalf("reaper options = %+v", ro)
	}
}

// A suspension written while a tenant starts, after its bound read the
// states and before it is served, reaches it once it is served: the read in
// between saw a change, but applied it only to the tenants served then.
func TestSuspensionWhileATenantStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	s := &shard{log: slog.New(slog.DiscardHandler), cfg: &config.Config{}}
	live := &liveTenants{}
	if err := s.watchStates(ctx, &config.Config{TenantState: path}, live, s.log); err != nil {
		t.Fatal(err)
	}
	acme := tenant{ID: "acme"}
	st := &servedTenant{t: acme, bound: s.newBound(acme, s.log)} // starting
	if code, _, _ := runTenant(t, "suspend", "acme", "--reason", "abuse", "--state", path); code != 0 {
		t.Fatal("suspend failed")
	}
	if err := s.states.Read(ctx); err != nil { // the change, while acme is not served yet
		t.Fatal(err)
	}
	if susp, _ := st.bound.State(); susp {
		t.Fatal("the test needs the read to miss the starting tenant")
	}
	live.add(st)
	if susp, why := st.bound.State(); !susp || !strings.Contains(why, "abuse") {
		t.Fatalf("served without its suspension: %v %q", susp, why)
	}
}

type brokenStore struct{}

func (brokenStore) Load(context.Context) (tenantstate.States, error) {
	return nil, context.DeadlineExceeded
}
func (brokenStore) Put(context.Context, string, tenantstate.Record) error { return nil }
func (brokenStore) Delete(context.Context, string) error                  { return nil }
func (brokenStore) String() string                                        { return "broken" }

// On the daemon's host, with no FR_* variable set, the tenant command reads the
// state store from the daemon's settings file, as status and doctor do: an
// operator on the control VM need not look the store up.
func TestTenantCommandReadsTheDaemonsEnvFile(t *testing.T) {
	clearFREnv(t)
	path := filepath.Join(t.TempDir(), "tenant-state.json")
	if code, out, errOut := runTenant(t, "suspend", "acme", "--reason", "abuse report", "--state", path, "--env-file", "none"); code != 0 {
		t.Fatalf("suspend: exit %d\n%s%s", code, out, errOut)
	}
	clearFREnv(t)
	old := daemonEnvFile
	t.Cleanup(func() { daemonEnvFile = old })
	daemonEnvFile = writeEnv(t, "FR_TENANT_STATE="+path+"\n")
	code, out, errOut := runTenant(t, "list", "--json")
	var r tenantReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || len(r.Suspended) != 1 || r.Suspended[0].ID != "acme" {
		t.Fatalf("list with only the daemon's settings file: exit %d, %+v, %v\n%s%s", code, r, err, out, errOut)
	}
	if !strings.Contains(errOut, "read 1 settings from "+daemonEnvFile) {
		t.Fatalf("stderr = %q, want it to say what it read", errOut)
	}
}
