package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clearFREnv unsets every FR_* variable for the test, restoring them after.
func clearFREnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "FR_") {
			t.Setenv(k, "") // registers the restore
			os.Unsetenv(k)
		}
	}
}

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "firerunner.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// With no FR_* variable set, status and doctor read the daemon's own
// settings, and say so: run from a shell on the host, they would otherwise
// describe a default deployment rather than this one.
func TestDiagReadsTheDaemonsEnvFile(t *testing.T) {
	clearFREnv(t)
	old := daemonEnvFile
	t.Cleanup(func() { daemonEnvFile = old })
	daemonEnvFile = writeEnv(t, "# the daemon's\nFR_URL=https://github.com/acme\n\nFR_TIERS='/etc/firerunner/tiers.json'\n")
	t.Setenv("FR_URL", "")
	os.Unsetenv("FR_URL")
	t.Setenv("FR_TIERS", "")
	os.Unsetenv("FR_TIERS")

	var stderr bytes.Buffer
	rest, err := diagEnv("status", []string{"--json"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rest, []string{"--json"}) {
		t.Errorf("args = %q", rest)
	}
	if os.Getenv("FR_URL") != "https://github.com/acme" || os.Getenv("FR_TIERS") != "/etc/firerunner/tiers.json" {
		t.Errorf("FR_URL=%q FR_TIERS=%q, want the file's (quotes removed)", os.Getenv("FR_URL"), os.Getenv("FR_TIERS"))
	}
	if !strings.Contains(stderr.String(), "read 2 settings from "+daemonEnvFile) {
		t.Errorf("stderr = %q, want it to say what it read", stderr.String())
	}
}

// Any FR_* variable already set means the caller chose its settings: the
// daemon's file is left alone.
func TestDiagLeavesAnEnvironmentAlone(t *testing.T) {
	clearFREnv(t)
	old := daemonEnvFile
	t.Cleanup(func() { daemonEnvFile = old })
	daemonEnvFile = writeEnv(t, "FR_URL=https://github.com/acme\n")
	t.Setenv("FR_LOG_LEVEL", "debug")
	t.Setenv("FR_URL", "")
	os.Unsetenv("FR_URL")
	var stderr bytes.Buffer
	if _, err := diagEnv("doctor", nil, &stderr); err != nil {
		t.Fatal(err)
	}
	if v, set := os.LookupEnv("FR_URL"); set {
		t.Errorf("FR_URL = %q, read although FR_LOG_LEVEL was set", v)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// --env-file reads the named file, never overriding a variable already set;
// --env-file none reads nothing; a malformed line is an error naming it.
func TestDiagEnvFileFlag(t *testing.T) {
	clearFREnv(t)
	p := writeEnv(t, "FR_URL=https://github.com/acme\nFR_NAME=from-file\n")
	t.Setenv("FR_NAME", "from-env")
	t.Setenv("FR_URL", "")
	os.Unsetenv("FR_URL")
	var stderr bytes.Buffer
	rest, err := diagEnv("status", []string{"--env-file", p, "--json"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rest, []string{"--json"}) || os.Getenv("FR_URL") != "https://github.com/acme" || os.Getenv("FR_NAME") != "from-env" {
		t.Errorf("rest %q FR_URL %q FR_NAME %q", rest, os.Getenv("FR_URL"), os.Getenv("FR_NAME"))
	}

	old := daemonEnvFile
	t.Cleanup(func() { daemonEnvFile = old })
	daemonEnvFile = writeEnv(t, "FR_TIERS=x\n")
	clearFREnv(t)
	t.Setenv("FR_TIERS", "")
	os.Unsetenv("FR_TIERS")
	if _, err := diagEnv("status", []string{"--env-file=none"}, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("FR_TIERS"); set {
		t.Error("--env-file none read the daemon's file")
	}

	bad := writeEnv(t, "FR_URL=x\nnot a setting\n")
	if _, err := diagEnv("status", []string{"--env-file", bad}, &stderr); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Errorf("malformed file: %v, want an error naming line 2", err)
	}
	if _, err := diagEnv("status", []string{"--env-file"}, &stderr); err == nil {
		t.Error("--env-file without a value: want an error")
	}
}
