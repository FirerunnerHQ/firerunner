package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func runConfig(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := configCmd(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestConfigCheckCatalog(t *testing.T) {
	t.Setenv("FR_MAX_RUNNERS", "")
	code, out, _ := runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json", "--max-runners", "8")
	if code != 0 || !strings.Contains(out, "valid (catalog): 2 tier(s)") || !strings.Contains(out, "c8a.xlarge") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// An over-budget catalog is invalid, exit 1, and the message names the fix.
	code, out, _ = runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json", "--max-runners", "2")
	if code != 1 || !strings.Contains(out, "--max-runners") {
		t.Fatalf("over budget: exit %d\n%s", code, out)
	}
	// The budget defaults like the daemon's, from FR_MAX_RUNNERS.
	t.Setenv("FR_MAX_RUNNERS", "8")
	if code, out, _ := runConfig(t, "check", "--catalog=../../examples/tiers-ec2.json"); code != 0 {
		t.Fatalf("FR_MAX_RUNNERS default: exit %d\n%s", code, out)
	}
}

func TestConfigCheckJSON(t *testing.T) {
	code, out, _ := runConfig(t, "check", "--json", "--catalog", "../../examples/tiers-ec2.json", "--max-runners", "8")
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != 0 || !r.OK || r.Mode != "catalog" || len(r.Tiers) != 2 || r.Tiers[1].Provisioner != "ec2" || len(r.Notes) == 0 {
		t.Fatalf("report = %+v (exit %d)", r, code)
	}
	code, out, _ = runConfig(t, "check", "--json", "--catalog", "/no/such.json")
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 1 || r.OK || r.Error == "" {
		t.Fatalf("missing file: exit %d report %+v err %v", code, r, err)
	}
}

func TestConfigCheckDaemonNeverPrintsSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"check", "--url", "https://github.com/o", "--token", "ghp_SECRET", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"},
		{"check", "--json", "--url", "https://github.com/o", "--token", "ghp_SECRET", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"},
	} {
		code, out, errOut := runConfig(t, args...)
		if code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		if strings.Contains(out+errOut, "ghp_SECRET") {
			t.Fatalf("token printed:\n%s", out)
		}
		if !strings.Contains(out, "token") {
			t.Fatalf("auth kind missing:\n%s", out)
		}
	}
	// A config the daemon would refuse is invalid here too.
	code, out, _ := runConfig(t, "check", "--token", "t", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0")
	if code != 1 || !strings.Contains(out, "--url is required") {
		t.Fatalf("missing url: exit %d\n%s", code, out)
	}
}

func TestConfigCheckUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"lint"},
		{"check", "--catalog"},
		{"check", "--catalog", "x.json", "--url", "u"},
		{"check", "--catalog", "x.json", "--max-runners", "many"},
	} {
		if code, _, _ := runConfig(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, out, _ := runConfig(t, "check", "--help"); code != 0 || !strings.Contains(out, "exit status") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
}
