package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/migrate"
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
		// One label to GitHub, mapped two ways, with or without a catalog.
		{"--repo", "acme/app", "--map", "Ubuntu-22.04=firerunner", "--map", "ubuntu-22.04=node"},
		{"--repo", "acme/app", "--catalog", "../../examples/tiers.json", "--map", "Ubuntu-22.04=firerunner", "--map", "ubuntu-22.04=node"},
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
	srv, writes := fakeRepoAPI(t, "jobs:\n  a:\n    runs-on: ubuntu-latest\n  b:\n    runs-on: ubuntu-22.04\n")
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
	if len(*writes) != 0 {
		t.Errorf("a dry run wrote %v", *writes)
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

func writeCatalog(t *testing.T, tiers string) string {
	t.Helper()
	p := t.TempDir() + "/tiers.json"
	if err := os.WriteFile(p, []byte(tiers), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const replacesCatalog = `[
  {"name": "fr-small", "provisioner": "ec2", "max": 2, "replaces": ["ubuntu-latest", "ubuntu-24.04"],
   "ec2": {"ami": "ami-0aaa", "instance_types": ["c7a.large"], "subnets": ["subnet-0aaa"], "security_groups": ["sg-0aaa"], "market": "on-demand"}},
  {"name": "fr-big", "labels": ["big"], "provisioner": "ec2", "max": 2,
   "ec2": {"ami": "ami-0aaa", "instance_types": ["c7a.4xlarge"], "subnets": ["subnet-0aaa"], "security_groups": ["sg-0aaa"], "market": "on-demand"}}
]`

func TestCatalogMap(t *testing.T) {
	p := writeCatalog(t, replacesCatalog)
	got, err := catalogMap(p, nil)
	if err != nil || !reflect.DeepEqual(got, migrate.Map{"ubuntu-latest": "fr-small", "ubuntu-24.04": "fr-small"}) {
		t.Errorf("catalog alone: %v, %v", got, err)
	}
	// --map overrides a replaced label (in any case) and adds others.
	got, err = catalogMap(p, migrate.Map{"Ubuntu-24.04": "big", "macos-latest": "fr-big"})
	if err != nil || !reflect.DeepEqual(got, migrate.Map{"ubuntu-latest": "fr-small", "Ubuntu-24.04": "big", "macos-latest": "fr-big"}) {
		t.Errorf("with --map: %v, %v", got, err)
	}
	// --map X=X keeps X where it runs, even when the catalog replaces it.
	got, err = catalogMap(p, migrate.Map{"UBUNTU-LATEST": "ubuntu-latest"})
	if err != nil || !reflect.DeepEqual(got, migrate.Map{"ubuntu-24.04": "fr-small"}) {
		t.Errorf("opt out: %v, %v", got, err)
	}
	// Only ASCII case folds: --map ubuntſ-latest (long s) does not override
	// the catalog's ubuntu-latest.
	got, err = catalogMap(p, migrate.Map{"ubuntſ-latest": "fr-big"})
	if err != nil || got["ubuntu-latest"] != "fr-small" || got["ubuntſ-latest"] != "fr-big" {
		t.Errorf("non-ASCII fold: %v, %v", got, err)
	}
	if _, err := catalogMap(p, migrate.Map{"ubuntu-latest": "fr-nope"}); err == nil {
		t.Error("a target no tier answers to is accepted")
	}
	none := writeCatalog(t, strings.ReplaceAll(replacesCatalog, `"replaces": ["ubuntu-latest", "ubuntu-24.04"],`, ""))
	if _, err := catalogMap(none, nil); err == nil || !strings.Contains(err.Error(), "no tier sets replaces") {
		t.Errorf("no replaces, no --map: %v", err)
	}
	bad := writeCatalog(t, strings.ReplaceAll(replacesCatalog, `"ubuntu-24.04"]`, `"big"]`))
	if _, err := catalogMap(bad, nil); err == nil {
		t.Error("an invalid replaces is accepted")
	}
}

// --catalog alone opens the same rewrite as the explicit --map it implies.
func TestMigrateCatalogOnly(t *testing.T) {
	t.Setenv("GH_TOKEN", "unused")
	if code, _, errOut := runMigrate(t, "--repo", "acme/app"); code != 2 || !strings.Contains(errOut, "--catalog") {
		t.Errorf("neither --map nor --catalog: exit %d, %s", code, errOut)
	}
	if code, _, errOut := runMigrate(t, "--repo", "acme/app", "--catalog", writeCatalog(t, strings.ReplaceAll(replacesCatalog, `"replaces": ["ubuntu-latest", "ubuntu-24.04"],`, ""))); code != 2 || !strings.Contains(errOut, "replaces") {
		t.Errorf("catalog without replaces: exit %d, %s", code, errOut)
	}
}

// fakeRepoAPI serves acme/app, with one workflow ci.yml holding wf, for
// dry runs; writes collects any write request.
func fakeRepoAPI(t *testing.T, wf string) (*httptest.Server, *[]string) {
	t.Helper()
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
	t.Cleanup(srv.Close)
	return srv, &writes
}

// --catalog alone opens the same rewrite as the explicit --map it implies.
func TestMigrateCatalogEqualsExplicitMap(t *testing.T) {
	srv, _ := fakeRepoAPI(t, "jobs:\n  a:\n    runs-on: ubuntu-latest\n  b:\n    runs-on: [ubuntu-24.04]\n  c:\n    runs-on: macos-latest\n")
	t.Setenv("GH_TOKEN", "tok")
	cat := writeCatalog(t, replacesCatalog)
	report := func(args ...string) any {
		code, out, errOut := runMigrate(t, append([]string{"--repo", "acme/app", "--api-url", srv.URL, "--dry-run", "--json"}, args...)...)
		if code != 0 {
			t.Fatalf("%q: exit %d: %s", args, code, errOut)
		}
		var r struct {
			Map   map[string]string `json:"map"`
			Files any               `json:"files"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	fromCatalog := report("--catalog", cat)
	explicit := report("--map", "ubuntu-latest=fr-small", "--map", "ubuntu-24.04=fr-small")
	if !reflect.DeepEqual(fromCatalog, explicit) {
		t.Errorf("--catalog alone:\n%+v\nexplicit --map:\n%+v", fromCatalog, explicit)
	}
}
