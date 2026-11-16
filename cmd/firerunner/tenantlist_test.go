package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

type fakeTenantList struct {
	list    []byte
	written []jobstore.Entry
	closed  bool
}

func (f *fakeTenantList) Load(context.Context) ([]byte, error) { return f.list, nil }
func (f *fakeTenantList) Replace(_ context.Context, e []jobstore.Entry) error {
	f.written = e
	return nil
}
func (f *fakeTenantList) Close() { f.closed = true }

// listInstall sets the environment of an install that may serve tenants: an
// App, and a catalog with the EC2 tier "firerunner-ubuntu-24.04-4".
func listInstall(t *testing.T) {
	t.Helper()
	t.Setenv("FR_URL", "https://github.com/acme-install")
	t.Setenv("FR_APP_CLIENT_ID", "Iv1.test")
	t.Setenv("FR_APP_PRIVATE_KEY", "unused")
	t.Setenv("FR_TIERS", filepath.Join("..", "..", "examples", "tiers-ec2.json"))
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://u@127.0.0.1:1/db")
}

func runTenantList(t *testing.T, store *fakeTenantList, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := tenantCmd(append(args, "--env-file", "none"), &out, &errOut, tenantOpeners{
		list: func(*config.Config) (tenantListStore, error) { return store, nil },
	})
	return code, out.String(), errOut.String()
}

// import writes the list it was given, each entry under its id, once it
// passes the daemon's own check; export prints the stored list.
func TestTenantImportExport(t *testing.T) {
	listInstall(t)
	file := filepath.Join(t.TempDir(), "tenants.json")
	list := `[{"id":"b","url":"https://github.com/b","installation_id":2,"tiers":["firerunner-ubuntu-24.04-4"]},
	          {"id":"a","url":"https://github.com/a","installation_id":1,"tiers":["firerunner-ubuntu-24.04-4"],"limits":{"max_runners":1}}]`
	if err := os.WriteFile(file, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeTenantList{}
	code, out, errOut := runTenantList(t, store, "import", file)
	if code != 0 || !strings.Contains(out, "wrote 2 tenant(s)") || !store.closed {
		t.Fatalf("import: exit %d\n%s%s", code, out, errOut)
	}
	if len(store.written) != 2 || store.written[0].ID != "b" || store.written[1].ID != "a" ||
		!strings.Contains(string(store.written[1].Entry), `"max_runners":1`) {
		t.Fatalf("wrote %+v", store.written)
	}

	store = &fakeTenantList{list: []byte(`[{"id": "a"}]`)}
	if code, out, errOut := runTenantList(t, store, "export"); code != 0 || out != "[\n  {\n    \"id\": \"a\"\n  }\n]\n" {
		t.Fatalf("export: exit %d\n%q%s", code, out, errOut)
	}
}

// A list the daemon would refuse is never written: the store is not even
// opened.
func TestTenantImportRefusesABadList(t *testing.T) {
	listInstall(t)
	dir := t.TempDir()
	for name, list := range map[string]string{
		"not a list":      `{"id":"a"}`,
		"unknown tier":    `[{"id":"a","url":"https://github.com/a","installation_id":1,"tiers":["huge"]}]`,
		"same account":    `[{"id":"a","url":"https://github.com/a","installation_id":1,"tiers":["firerunner-ubuntu-24.04-4"]},{"id":"b","url":"https://github.com/a/r","installation_id":2,"tiers":["firerunner-ubuntu-24.04-4"]}]`,
		"unknown field":   `[{"id":"a","url":"https://github.com/a","installation_id":1,"tiers":["firerunner-ubuntu-24.04-4"],"shard":"x"}]`,
		"bad id":          `[{"id":"A_b","url":"https://github.com/a","installation_id":1,"tiers":["firerunner-ubuntu-24.04-4"]}]`,
		"no installation": `[{"id":"a","url":"https://github.com/a","tiers":["firerunner-ubuntu-24.04-4"]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(file, []byte(list), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			code := tenantCmd([]string{"import", file, "--env-file", "none"}, &out, &errOut, tenantOpeners{
				list: func(*config.Config) (tenantListStore, error) { t.Fatal("store opened for a bad list"); return nil, nil },
			})
			if code != 1 {
				t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errOut.String())
			}
		})
	}
}

// import takes one file, export none.
func TestTenantListUsage(t *testing.T) {
	listInstall(t)
	for _, args := range [][]string{{"import"}, {"import", "a", "b"}, {"export", "x"}} {
		if code, _, _ := runTenantList(t, &fakeTenantList{}, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
