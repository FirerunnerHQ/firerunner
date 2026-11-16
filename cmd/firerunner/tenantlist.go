package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// tenantListStore is the job store's tenants table; a *jobstore.TenantStore.
type tenantListStore interface {
	Load(ctx context.Context) ([]byte, error)
	Replace(ctx context.Context, entries []jobstore.Entry) error
	Close()
}

// openTenantList connects to the job store the daemon's settings name.
func openTenantList(cfg *config.Config) (tenantListStore, error) {
	dsn, err := jobStoreDSN(cfg)
	if err != nil {
		return nil, err
	}
	if dsn == "" {
		return nil, fmt.Errorf("no job store: set FR_JOB_EVENTS_DSN or FR_JOB_EVENTS_DSN_FILE")
	}
	return jobstore.OpenTenants(dsn)
}

// tenantListCmd runs "firerunner tenant import FILE" and "tenant export":
// the tenants list of --tenants db:, written whole or printed. args start
// with the action; the env file has been read. open connects to the store
// cfg names.
func tenantListCmd(args []string, stdout, stderr io.Writer, open func(*config.Config) (tenantListStore, error)) int {
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n%s", append(a, tenantUsage)...)
		return 2
	}
	action := args[0]
	var file string
	switch {
	case action == "import" && len(args) == 2:
		file = args[1]
	case action == "export" && len(args) == 1:
	case action == "import":
		return usageErr("firerunner tenant import: name one FILE (- for stdin)")
	default:
		return usageErr("firerunner tenant export: takes no argument")
	}
	cfg, err := config.Parse(nil)
	if err != nil {
		return usageErr("firerunner tenant %s: %v", action, err)
	}
	var entries []jobstore.Entry
	if action == "import" {
		if entries, err = checkedTenantList(cfg, file); err != nil {
			fmt.Fprintf(stderr, "firerunner tenant import: %v\n", err)
			return 1
		}
	}
	store, err := open(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant %s: %v\n", action, err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if action == "import" {
		if err := store.Replace(ctx, entries); err != nil {
			fmt.Fprintf(stderr, "firerunner tenant import: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %d tenant(s); a daemon with --tenants db: serves them within 30s\n", len(entries))
		return 0
	}
	list, err := store.Load(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant export: %v\n", err)
		return 1
	}
	var out bytes.Buffer
	if err := json.Indent(&out, list, "", "  "); err != nil {
		fmt.Fprintf(stderr, "firerunner tenant export: %v\n", err)
		return 1
	}
	out.WriteByte('\n')
	_, _ = out.WriteTo(stdout)
	return 0
}

// checkedTenantList reads a tenants list and checks it as the daemon checks
// one it reads: a list the daemon would refuse is never written.
func checkedTenantList(cfg *config.Config, file string) ([]jobstore.Entry, error) {
	var b []byte
	var err error
	if file == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, err
	}
	tenants, err := config.ParseTenants(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := cfg.CheckTenants(tenants); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	entries := make([]jobstore.Entry, len(tenants))
	for i, t := range tenants {
		entries[i] = jobstore.Entry{ID: t.ID, Entry: raw[i]}
	}
	return entries, nil
}
