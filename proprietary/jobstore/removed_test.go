package jobstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A tenant deleted from the table, by Replace or by hand, is kept as
// removed until its leftovers are forgotten; listed again, it is not
// removed. Forgetting one removal leaves a later one of the same id.
func TestRemovedTenants(t *testing.T) {
	s, dsn := openTenantStore(t)
	ctx := context.Background()
	if got, err := s.RemovedTenants(ctx); err != nil || len(got) != 0 {
		t.Fatalf("before set-up: %v, %v", got, err)
	}
	if err := s.Replace(ctx, []Entry{entry("a", "https://github.com/a"), entry("b", "https://github.com/b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, []Entry{entry("b", "https://github.com/b")}); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `DELETE FROM tenants WHERE id = 'b'`)
	got, err := s.RemovedTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" || !jsonHas(got[0].Entry, `"url": "https://github.com/a"`) {
		t.Fatalf("removed %+v, want a then b, with their entries", got)
	}

	// Listed again: no longer removed.
	if err := s.Replace(ctx, []Entry{entry("b", "https://github.com/b")}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("removed %+v after b was listed again, want a alone", got)
	}

	// a removed again since the removal a sweep read: forgetting that
	// one leaves the new one.
	first := got[0].RemovedAt
	if err := s.Replace(ctx, []Entry{entry("a", "https://github.com/a"), entry("b", "https://github.com/b")}); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `DELETE FROM tenants WHERE id = 'a'`)
	if err := s.ForgetRemoved(ctx, "a", first, "https://github.com/a"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 || got[0].RemovedAt.Equal(first) {
		t.Fatalf("removed %+v, want a's later removal kept", got)
	}
	if err := s.ForgetRemoved(ctx, "a", got[0].RemovedAt, "https://github.com/elsewhere"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 {
		t.Fatalf("removed %+v: a removal on another URL was forgotten", got)
	}
	if err := s.ForgetRemoved(ctx, "a", got[0].RemovedAt, "https://github.com/a"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 0 {
		t.Fatalf("removed %+v once forgotten", got)
	}
	// One whose entry does not read: forgotten by its exact time alone.
	exec(t, dsn, `INSERT INTO tenants (id, entry) VALUES ('z', '{"id": "z"}')`)
	exec(t, dsn, `DELETE FROM tenants WHERE id = 'z'`)
	z, _ := s.RemovedTenants(ctx)
	if err := s.ForgetRemoved(ctx, "z", z[0].RemovedAt.Add(-time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 {
		t.Fatalf("removed %+v: forgot z at another time", got)
	}
	if err := s.ForgetRemoved(ctx, "z", z[0].RemovedAt, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 0 {
		t.Fatalf("removed %+v, want z forgotten by its time", got)
	}

	// A removal recorded by now, forgotten by a run that saw it then.
	exec(t, dsn, `DELETE FROM tenants WHERE id = 'b'`)
	if listed, err := s.TenantListed(ctx, "b"); err != nil || listed {
		t.Fatalf("TenantListed(b) = %v, %v once deleted", listed, err)
	}
	if listed, err := s.TenantListed(ctx, "a"); err != nil || listed {
		t.Fatalf("TenantListed(a) = %v, %v", listed, err)
	}
	if err := s.ForgetRemoved(ctx, "b", time.Now().Add(time.Minute), "https://github.com/b"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 0 {
		t.Fatalf("removed %+v, want b forgotten", got)
	}

	// An id changed in place: the old one is removed, the new one is not.
	if err := s.Replace(ctx, []Entry{entry("c", "https://github.com/c")}); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE tenants SET id = 'd', entry = jsonb_set(entry, '{id}', '"d"') WHERE id = 'c'`)
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("removed %+v after c was renamed d, want c", got)
	}
	if listed, err := s.TenantListed(ctx, "d"); err != nil || !listed {
		t.Fatalf("TenantListed(d) = %v, %v", listed, err)
	}
	exec(t, dsn, `UPDATE tenants SET entry = jsonb_set(entry, '{url}', '"https://github.com/e"') WHERE id = 'd'`)
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 {
		t.Fatalf("removed %+v after d's entry changed, want c alone", got)
	}
}

func jsonHas(b []byte, s string) bool {
	return strings.Contains(strings.ReplaceAll(string(b), `":"`, `": "`), s)
}
