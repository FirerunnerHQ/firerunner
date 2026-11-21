package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A database nobody set up has no entitlements table: the reader says so;
// a writer does not set it up, as whoever did would own the job store's
// tables (billing's role, say, which the daemon's then could not write),
// and says the daemon does; clearing a record there is no error.
func TestEntitlementsBeforeSetUp(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if _, err := s.Entitlements(ctx); !errors.Is(err, ErrNoEntitlements) {
		t.Fatalf("Entitlements = %v, want ErrNoEntitlements", err)
	}
	err := s.PutEntitlement(ctx, "acme", json.RawMessage(`{}`))
	if !errors.Is(err, ErrNoEntitlements) || !strings.Contains(err.Error(), "start the daemon") {
		t.Fatalf("PutEntitlement before set-up = %v, want ErrNoEntitlements naming the daemon", err)
	}
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoTenants) {
		t.Fatalf("after PutEntitlement, the tenants table: %v; want the database left as it was", err)
	}
	if err := s.DeleteEntitlement(ctx, "acme"); err != nil {
		t.Fatalf("DeleteEntitlement before set-up = %v", err)
	}
}

// setUp sets the database up, as the daemon does when it starts.
func setUp(t *testing.T, s *TenantStore) {
	t.Helper()
	if err := prepare(context.Background(), s.pool); err != nil {
		t.Fatal(err)
	}
}

// PutEntitlement writes one tenant's row
// and leaves the others; the rows come back by tenant, as written, with
// when each was last written; DeleteEntitlement removes one.
func TestEntitlementsPutLoadDelete(t *testing.T) {
	s, _ := openTenantStore(t)
	setUp(t, s)
	ctx := context.Background()
	if err := s.PutEntitlement(ctx, "zeta", json.RawMessage(`{"account_state":"trial"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEntitlement(ctx, "acme", json.RawMessage(`{"account_state":"active"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEntitlement(ctx, "acme", json.RawMessage(`{"account_state":"suspended"}`)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Entitlements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Tenant != "acme" || rows[1].Tenant != "zeta" {
		t.Fatalf("rows = %+v, want acme then zeta", rows)
	}
	var rec map[string]string
	if err := json.Unmarshal(rows[0].Record, &rec); err != nil || rec["account_state"] != "suspended" {
		t.Fatalf("acme's record = %s (%v), want the last written", rows[0].Record, err)
	}
	if rows[0].UpdatedAt.IsZero() || time.Since(rows[0].UpdatedAt) > time.Minute {
		t.Fatalf("acme's updated_at = %v, want now", rows[0].UpdatedAt)
	}
	if err := s.DeleteEntitlement(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEntitlement(ctx, "acme"); err != nil {
		t.Fatalf("deleting a row already gone = %v", err)
	}
	if rows, err := s.Entitlements(ctx); err != nil || len(rows) != 1 || rows[0].Tenant != "zeta" {
		t.Fatalf("after delete: %+v, %v; want zeta alone", rows, err)
	}
}

// Billing writes the rows with SQL of its own, which need not set
// updated_at: the table sets it on every insert and update, so a reader
// always sees when a record changed.
func TestEntitlementsWrittenBySQL(t *testing.T) {
	s, dsn := openTenantStore(t)
	setUp(t, s)
	ctx := context.Background()
	if err := s.PutEntitlement(ctx, "acme", json.RawMessage(`{"account_state":"active"}`)); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := conn.Exec(ctx, `INSERT INTO entitlements (tenant, record, updated_at) VALUES ('beta', '{}', $1)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE entitlements SET record = '{"account_state":"suspended"}', updated_at = $1 WHERE tenant = 'acme'`, old); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Entitlements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if !r.UpdatedAt.After(old) {
			t.Errorf("%s: updated_at = %v, want the time of the write, not the one given", r.Tenant, r.UpdatedAt)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO entitlements (tenant, record) VALUES ('', '{}')`); err == nil {
		t.Error("a row with no tenant was accepted")
	}
}

// A role granted SELECT alone, as the daemon's should be once billing owns
// the table, reads the rows and may not write one: Postgres refuses it with
// insufficient_privilege, which the command explains.
func TestEntitlementsReadOnlyRole(t *testing.T) {
	s, dsn := openTenantStore(t)
	setUp(t, s)
	ctx := context.Background()
	if err := s.PutEntitlement(ctx, "acme", json.RawMessage(`{"account_state":"active"}`)); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	const role = "fr_test_entitlement_reader"
	if _, err := conn.Exec(ctx, `DROP ROLE IF EXISTS `+role+`; CREATE ROLE `+role+` LOGIN PASSWORD 'reader'; GRANT USAGE ON SCHEMA public TO `+role+`; GRANT SELECT ON entitlements TO `+role); err != nil {
		t.Skipf("cannot create a role here: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE IF EXISTS `+role)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "reader")
	reader, err := OpenTenants(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if rows, err := reader.Entitlements(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("the reader's Entitlements = %+v, %v; want acme's row", rows, err)
	}
	err = reader.PutEntitlement(ctx, "acme", json.RawMessage(`{"account_state":"active"}`))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("the reader's PutEntitlement = %v, want insufficient_privilege naming the tenant", err)
	}
}
