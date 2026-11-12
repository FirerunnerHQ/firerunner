package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

func runEntitlement(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := entitlementCmd(args, &out, &errOut, func(spec string) (entitlement.Store, error) {
		return entitlement.Open(spec, nil)
	})
	return code, out.String(), errOut.String()
}

// set writes a record the daemon reads, and says what it allows; list shows
// each and its decision; clear removes it, leaving the tenant held.
func TestEntitlementCommand(t *testing.T) {
	clearFREnv(t)
	path := filepath.Join(t.TempDir(), "entitlements.json")
	code, out, errOut := runEntitlement(t, "set", "acme", "--account", "active", "--budget", "capped", "--reason", "November's cap", "--store", path, "--env-file", "none")
	if code != 0 || !strings.Contains(out, "is held: its spend cap is reached: November's cap") {
		t.Fatalf("set: exit %d\n%s%s", code, out, errOut)
	}
	rs, err := (&entitlement.FileStore{Path: path}).Load(context.Background())
	if err != nil || rs["acme"].Budget != entitlement.Capped || rs["acme"].ValidUntil.IsZero() {
		t.Fatalf("stored %+v, %v", rs, err)
	}
	code, out, errOut = runEntitlement(t, "set", "beta", "--account", "trial", "--budget", "ok", "--valid-for", "1h", "--max-runners", "2", "--tiers", "small,medium", "--store", path, "--env-file", "none")
	if code != 0 || !strings.Contains(out, "budget ok, at most 2 runner(s), tiers small,medium") {
		t.Fatalf("set beta: exit %d\n%s%s", code, out, errOut)
	}
	if rs, _ := (&entitlement.FileStore{Path: path}).Load(context.Background()); rs["beta"].Limits == nil ||
		rs["beta"].Limits.MaxRunners != 2 || !slices.Equal(rs["beta"].Limits.Tiers, []string{"small", "medium"}) {
		t.Fatalf("stored beta %+v", rs["beta"])
	}
	code, out, _ = runEntitlement(t, "list", "--store", path, "--env-file", "none", "--json")
	var r entitlementReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || len(r.Records) != 2 {
		t.Fatalf("list --json: exit %d, %+v, %v\n%s", code, r, err, out)
	}
	if r.Records[0].ID != "acme" || !r.Records[0].Decision.Held || r.Records[1].ID != "beta" || r.Records[1].Decision.Held {
		t.Fatalf("list = %+v", r.Records)
	}
	if code, out, _ := runEntitlement(t, "clear", "acme", "--store", path, "--env-file", "none"); code != 0 || !strings.Contains(out, "holds its launches") {
		t.Fatalf("clear: exit %d\n%s", code, out)
	}
	if rs, _ := (&entitlement.FileStore{Path: path}).Load(context.Background()); len(rs) != 1 {
		t.Fatalf("after clear: %+v", rs)
	}
}

// Mistakes are usage errors that write nothing.
func TestEntitlementCommandUsage(t *testing.T) {
	clearFREnv(t)
	path := filepath.Join(t.TempDir(), "entitlements.json")
	for name, args := range map[string][]string{
		"bad account":          {"set", "acme", "--account", "paid", "--budget", "ok"},
		"no budget":            {"set", "acme", "--account", "active"},
		"the install":          {"set", "_install", "--account", "active", "--budget", "ok"},
		"two ids":              {"clear", "a", "b"},
		"list with an id":      {"list", "acme"},
		"budget for clear":     {"clear", "acme", "--budget", "ok"},
		"zero validity":        {"set", "acme", "--account", "active", "--budget", "ok", "--valid-for", "0s"},
		"unknown action":       {"grant", "acme"},
		"unknown flag":         {"list", "--all"},
		"relative store":       {"list", "--store", "entitlements.json"},
		"no runners":           {"set", "acme", "--account", "active", "--budget", "ok", "--max-runners", "0"},
		"runners not a number": {"set", "acme", "--account", "active", "--budget", "ok", "--max-runners", "two"},
		"an empty tier":        {"set", "acme", "--account", "active", "--budget", "ok", "--tiers", "small,"},
		"tiers for list":       {"list", "--tiers", "small"},
	} {
		args := append(args, "--env-file", "none")
		if !strings.Contains(strings.Join(args, " "), "--store") {
			args = append(args, "--store", path)
		}
		if code, out, errOut := runEntitlement(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2\n%s%s", name, code, out, errOut)
		}
	}
	if rs, _ := (&entitlement.FileStore{Path: path}).Load(context.Background()); len(rs) != 0 {
		t.Fatalf("a usage error wrote %+v", rs)
	}
	if code, _, errOut := runEntitlement(t, "list", "--env-file", "none"); code != 2 || !strings.Contains(errOut, "FR_ENTITLEMENTS") {
		t.Fatalf("no store: exit %d\n%s", code, errOut)
	}
}
