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
	if err := s.ForgetRemoved(ctx, "a", first); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 1 || got[0].RemovedAt.Equal(first) {
		t.Fatalf("removed %+v, want a's later removal kept", got)
	}
	if err := s.ForgetRemoved(ctx, "a", got[0].RemovedAt); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 0 {
		t.Fatalf("removed %+v once forgotten", got)
	}
	exec(t, dsn, `DELETE FROM tenants WHERE id = 'b'`)
	if err := s.ForgetRemoved(ctx, "b", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.RemovedTenants(ctx); len(got) != 0 {
		t.Fatalf("removed %+v, want b forgotten whatever its time", got)
	}
}

func jsonHas(b []byte, s string) bool { return strings.Contains(strings.ReplaceAll(string(b), `":"`, `": "`), s) }
