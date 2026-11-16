package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// openTenantStore connects to FR_TEST_POSTGRES_URL with an empty schema: not
// even the store's tables.
func openTenantStore(t *testing.T) (*TenantStore, string) {
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
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	s, err := OpenTenants(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, dsn
}

func entry(id, url string) Entry {
	return Entry{ID: id, Entry: json.RawMessage(`{"id":"` + id + `","url":"` + url + `","installation_id":7,"tiers":["small"]}`)}
}

// A database nobody set up has no tenants table: the reader says so, which
// the daemon treats as a list that does not exist yet.
func TestTenantsBeforeSetUp(t *testing.T) {
	s, _ := openTenantStore(t)
	if _, err := s.Load(context.Background()); !errors.Is(err, ErrNoTenants) {
		t.Fatalf("Load = %v, want ErrNoTenants", err)
	}
}

// Replace writes the whole list: it sets the database up, adds, rewrites only
// what changed, and deletes what it leaves out. Load gives the entries back
// as one list, ordered by id.
func TestTenantsReplaceAndLoad(t *testing.T) {
	s, dsn := openTenantStore(t)
	ctx := context.Background()
	if err := s.Replace(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(ctx); err != nil || string(got) != "[]" {
		t.Fatalf("Load of an empty list = %s, %v", got, err)
	}
	if err := s.Replace(ctx, []Entry{entry("b", "https://github.com/b"), entry("a", "https://github.com/a")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(got, &list); err != nil || len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("Load = %s (%v), want a then b", got, err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	updated := func(id string) time.Time {
		t.Helper()
		var at time.Time
		if err := conn.QueryRow(ctx, `SELECT updated_at FROM tenants WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	aWas, bWas := updated("a"), updated("b")
	// b changes, a does not, c is new; then a is left out.
	if err := s.Replace(ctx, []Entry{entry("a", "https://github.com/a"), entry("b", "https://github.com/b2"), entry("c", "https://github.com/c")}); err != nil {
		t.Fatal(err)
	}
	if !updated("a").Equal(aWas) || !updated("b").After(bWas) {
		t.Fatalf("updated_at: a %v -> %v (unchanged entry), b %v -> %v (changed)", aWas, updated("a"), bWas, updated("b"))
	}
	if err := s.Replace(ctx, []Entry{entry("b", "https://github.com/b2"), entry("c", "https://github.com/c")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx); strings.Contains(string(got), `"id": "a"`) || !strings.Contains(string(got), `"https://github.com/b2"`) {
		t.Fatalf("Load = %s, want b2 and c only", got)
	}
}

// An entry whose own id differs from the id it is written under is refused:
// the row's key and the list's id cannot disagree.
func TestTenantsRefuseAMismatchedID(t *testing.T) {
	s, _ := openTenantStore(t)
	e := entry("a", "https://github.com/a")
	e.ID = "b"
	if err := s.Replace(context.Background(), []Entry{e}); err == nil {
		t.Fatal("Replace wrote an entry under another id")
	}
}
