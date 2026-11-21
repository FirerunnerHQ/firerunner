package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// scratchDB creates a database of its own on FR_TEST_POSTGRES_URL's server,
// dropped after the test, and returns its URL: the job store's tests reset
// that server's public schema, maybe at the same time.
func scratchDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("FR_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("FR_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "fr_cmd_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		conn.Close(ctx)
		t.Skipf("cannot create a database here: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		conn.Close(context.Background())
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// The command writes and lists records in the job store's entitlements
// table with db:, at the database FR_JOB_EVENTS_DSN names, setting it up
// when nothing has yet; and it says what is missing when no URL is set.
func TestEntitlementCommandDB(t *testing.T) {
	db := scratchDB(t) // before clearFREnv unsets FR_TEST_POSTGRES_URL
	clearFREnv(t)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := entitlementCmd(args, &out, &errOut, openEntitlements)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("list", "--store", "db:", "--env-file", "none"); code != 1 || !strings.Contains(errOut, "FR_JOB_EVENTS_DSN") {
		t.Fatalf("db: with no URL: exit %d, %s", code, errOut)
	}
	if code, _, errOut := run("list", "--store", "db:entitlements", "--env-file", "none"); code != 2 || !strings.Contains(errOut, "db: alone") {
		t.Fatalf("db:entitlements: exit %d, %s", code, errOut)
	}

	t.Setenv("FR_JOB_EVENTS_DSN", db)
	code, out, errOut := run("set", "acme", "--account", "trial", "--budget", "ok", "--valid-for", "720h", "--store", "db:", "--env-file", "none")
	if code != 0 || !strings.Contains(out, "may launch") || !strings.Contains(out, "entitlements table") {
		t.Fatalf("set: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = run("list", "--store", "db:", "--env-file", "none", "--json")
	var r entitlementReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || len(r.Records) != 1 {
		t.Fatalf("list --json: exit %d, %v\n%s%s", code, err, out, errOut)
	}
	if e := r.Records[0]; e.ID != "acme" || e.Record.Account != "trial" || e.Decision.Held || e.WrittenAt.IsZero() {
		t.Fatalf("listed %+v", e)
	}
	if code, out, errOut := run("clear", "acme", "--store", "db:", "--env-file", "none"); code != 0 {
		t.Fatalf("clear: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, _ := run("list", "--store", "db:", "--env-file", "none"); code != 0 || !strings.Contains(out, "no entitlement record") {
		t.Fatalf("list after clear: exit %d\n%s", code, out)
	}
}
