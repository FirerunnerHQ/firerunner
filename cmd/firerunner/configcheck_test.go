package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
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
		{"check", "--catalog", "x.json", "--max-runners", "0"},
		{"check", "--bogus"},
		{"check", "--vcpu", "x"},
	} {
		if code, _, _ := runConfig(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, out, _ := runConfig(t, "check", "--help"); code != 0 || !strings.Contains(out, "exit status") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
}

func TestConfigCheckMatchesStartup(t *testing.T) {
	// A malformed FR_MAX_RUNNERS fails the daemon at startup; the catalog
	// check must not silently fall back to 4.
	t.Setenv("FR_MAX_RUNNERS", "abc")
	if code, _, errOut := runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json"); code != 2 || !strings.Contains(errOut, "FR_MAX_RUNNERS") {
		t.Fatalf("malformed FR_MAX_RUNNERS: exit %d %s", code, errOut)
	}
	t.Setenv("FR_MAX_RUNNERS", "")
	if code, out, _ := runConfig(t, "check", "--catalog", "../../examples/tiers-ec2.json", "--max-runners=8"); code != 0 {
		t.Fatalf("--max-runners=8: exit %d\n%s", code, out)
	}
	// Startup resolves the App key; a key file that does not exist is
	// invalid here too, and the message never contains the key's value.
	// (An empty FR_MAX_RUNNERS is itself malformed for the daemon.)
	t.Setenv("FR_MAX_RUNNERS", "4")
	code, out, _ := runConfig(t, "check", "--url", "https://github.com/o", "--app-client-id", "Iv1",
		"--app-installation-id", "1", "--app-private-key", "/no/such/key.pem", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0")
	if code != 1 || !strings.Contains(out, "does not exist") {
		t.Fatalf("missing app key: exit %d\n%s", code, out)
	}
}

// emptyKeyArgs is a daemon config whose App private key file is empty, the
// input that panicked the daemon in issue #78.
func emptyKeyArgs(t *testing.T) (args []string, keyPath string) {
	t.Helper()
	t.Setenv("FR_MAX_RUNNERS", "4")
	t.Setenv("FR_TOKEN", "")
	keyPath = filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--url", "https://github.com/o", "--app-client-id", "Iv1", "--app-installation-id", "1",
		"--app-private-key", keyPath, "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"}, keyPath
}

func TestConfigCheckRejectsEmptyAppKey(t *testing.T) {
	args, keyPath := emptyKeyArgs(t)
	want := "--app-private-key " + keyPath + ": file is empty"

	code, out, _ := runConfig(t, append([]string{"check"}, args...)...)
	if code != 1 || !strings.HasPrefix(out, "invalid (daemon): "+want) {
		t.Fatalf("text: exit %d\n%s", code, out)
	}
	code, out, _ = runConfig(t, append([]string{"check", "--json"}, args...)...)
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 1 || r.OK || !strings.Contains(r.Error, want) || r.Auth != "" {
		t.Fatalf("json: exit %d report %+v err %v", code, r, err)
	}
}

// TestRunRejectsEmptyAppKey drives the daemon's startup path: an empty key
// file is an error naming the file, never a panic in the scale set client.
func TestRunRejectsEmptyAppKey(t *testing.T) {
	args, keyPath := emptyKeyArgs(t)
	cfg, err := config.FromFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = run(ctx, cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "--app-private-key "+keyPath+": file is empty") {
		t.Fatalf("run: %v", err)
	}
}

func TestConfigCheckUnknownFlagNeverPrintsSecrets(t *testing.T) {
	t.Setenv("FR_TOKEN", "ghp_SECRET")
	t.Setenv("FR_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----SECRET")
	out := captureStderrMain(t, func() {
		var o, e bytes.Buffer
		if code := configCmd([]string{"check", "--json", "--bogus"}, &o, &e); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
		if strings.Contains(o.String()+e.String(), "SECRET") {
			t.Errorf("credential in output:\n%s%s", o.String(), e.String())
		}
	})
	if strings.Contains(out, "SECRET") {
		t.Fatalf("credential on stderr:\n%s", out)
	}
}

// captureStderrMain returns what f wrote to os.Stderr, where the flag
// package prints usage.
func captureStderrMain(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}
