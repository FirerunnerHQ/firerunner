package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runMigrate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := migrateCmd(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestMigrateUsage(t *testing.T) {
	t.Setenv("GH_TOKEN", "unused")
	for _, args := range [][]string{
		{},
		{"--map", "ubuntu-latest=firerunner"},
		{"--repo", "acme/app"},
		{"--repo", "acme/app", "--map", "nonsense"},
		{"--repo", "acme/app", "--map", "a=b", "--map", "a=c"},
		{"--repo", "acme/app", "--map", "a=b", "--map", "b=c"},
		{"--repo", "acme/app", "--map", "a=true"},
		{"--repo", "acme/app", "--map", "a=b", "extra"},
		{"--repo", "acme/app", "--map", "a=b", "--bogus"},
		// The catalog has no tier of that name or label.
		{"--repo", "acme/app", "--map", "ubuntu-latest=fr-nope", "--catalog", "../../examples/tiers.json"},
		{"--repo", "acme/app", "--map", "ubuntu-latest=firerunner", "--catalog", "no-such-file.json"},
	} {
		code, out, errOut := runMigrate(t, args...)
		if code != 2 || out != "" || errOut == "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	if code, out, _ := runMigrate(t, "-h"); code != 0 || !strings.Contains(out, "--dry-run") {
		t.Errorf("-h: exit %d\n%s", code, out)
	}
}

// TestMigrateDryRunJSON runs the command against a one-workflow repository:
// the token from GH_TOKEN, a catalog name and label accepted as targets, the
// report on stdout, nothing written.
func TestMigrateDryRunJSON(t *testing.T) {
	const wf = "jobs:\n  a:\n    runs-on: ubuntu-latest\n  b:\n    runs-on: ubuntu-22.04\n"
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if r.Method != "GET" {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		var v any
		switch r.URL.Path {
		case "/repos/acme/app":
			v = map[string]string{"full_name": "acme/app", "default_branch": "main"}
		case "/repos/acme/app/commits/main":
			v = map[string]any{"sha": "c1", "commit": map[string]any{"tree": map[string]string{"sha": "t1"}}}
		case "/repos/acme/app/contents/.github":
			v = []map[string]string{{"name": "workflows", "type": "dir", "sha": "wf"}}
		case "/repos/acme/app/git/trees/wf":
			v = map[string]any{"tree": []map[string]string{{"path": "ci.yml", "mode": "100644", "type": "blob", "sha": "b1"}}}
		case "/repos/acme/app/pulls":
			v = []any{}
		case "/repos/acme/app/git/blobs/b1":
			v = map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(wf)), "encoding": "base64"}
		default:
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()
	t.Setenv("GH_TOKEN", "tok")

	code, out, errOut := runMigrate(t, "--repo", "acme/app", "--api-url", srv.URL, "--dry-run", "--json",
		"--catalog", "../../examples/tiers.json", "--map", "ubuntu-latest=firerunner", "--map", "ubuntu-22.04=node")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var rep struct {
		DryRun bool   `json:"dry_run"`
		PR     string `json:"pr"`
		Files  []struct {
			Path    string `json:"path"`
			Diff    string `json:"diff"`
			Changes []struct {
				Job, From, To string
			} `json:"changes"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not the JSON report: %v\n%s", err, out)
	}
	if !rep.DryRun || rep.PR != "none" || len(rep.Files) != 1 || len(rep.Files[0].Changes) != 2 ||
		!strings.Contains(rep.Files[0].Diff, "+    runs-on: node") {
		t.Errorf("report %+v", rep)
	}
	if len(writes) != 0 {
		t.Errorf("a dry run wrote %v", writes)
	}

	// The human form shows the diff and the plan.
	code, out, _ = runMigrate(t, "--repo", "acme/app", "--api-url", srv.URL, "--dry-run", "--map", "ubuntu-latest=firerunner")
	if code != 0 || !strings.Contains(out, "+    runs-on: firerunner") || !strings.Contains(out, `no mapping for label "ubuntu-22.04"`) ||
		!strings.Contains(out, "dry run: nothing pushed") {
		t.Errorf("exit %d\n%s", code, out)
	}

	// A refused token is a failure, exit 1, with the reason.
	t.Setenv("GH_TOKEN", "wrong")
	if code, _, errOut := runMigrate(t, "--repo", "acme/app", "--api-url", srv.URL, "--map", "ubuntu-latest=firerunner"); code != 1 || !strings.Contains(errOut, "401") {
		t.Errorf("bad token: exit %d, %s", code, errOut)
	}
}

// Under --json a failure is still JSON on stdout, even before any API call.
func TestMigrateJSONFailures(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no gh
	code, out, _ := runMigrate(t, "--repo", "acme/app", "--map", "a=b", "--json")
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if code != 1 || json.Unmarshal([]byte(out), &r) != nil || r.OK || !strings.Contains(r.Error, "no GitHub token") {
		t.Errorf("exit %d, stdout %q", code, out)
	}
	// The token is never sent over plain http, except to loopback.
	t.Setenv("GH_TOKEN", "tok")
	if code, _, errOut := runMigrate(t, "--repo", "acme/app", "--map", "a=b", "--api-url", "http://example.com"); code != 2 || !strings.Contains(errOut, "https") {
		t.Errorf("http api-url: exit %d, %s", code, errOut)
	}
}

func TestAPIHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":         "github.com",
		"https://API.GitHub.com/":        "github.com",
		"https://ghe.example.com/api/v3": "ghe.example.com",
		"http://127.0.0.1:8080":          "127.0.0.1:8080",
	} {
		if got, err := apiHost(in); err != nil || got != want {
			t.Errorf("%s: %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"http://api.github.com", "ftp://x", "nonsense"} {
		if _, err := apiHost(in); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}
