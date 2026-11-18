package jobstore

import (
	"context"
	"testing"
	"time"
)

// Installations read outside a delivery, and their runner groups' results
// recorded against them.
func TestInstallationGroups(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if _, ok, err := s.ReadInstallation(ctx, 7); err != nil || ok {
		t.Fatalf("before any: %v %v", ok, err)
	}
	for _, id := range []int64{8, 7} {
		if _, err := s.Onboard(ctx, "d"+string(rune('0'+id)), "installation", putTenant("t"+string(rune('0'+id)), id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Onboard(ctx, "s8", "installation", func(tx OnboardTx) (string, error) {
		in, _, err := tx.Installation(ctx, 8)
		in.State = "suspended"
		if err == nil {
			err = tx.PutInstallation(ctx, in)
		}
		return "", err
	}); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveInstallations(ctx)
	if err != nil || len(active) != 1 || active[0].ID != 7 || active[0].AccountLogin != "t7" {
		t.Fatalf("active %+v, %v", active, err)
	}
	if in, ok, err := s.ReadInstallation(ctx, 8); err != nil || !ok || in.State != "suspended" {
		t.Fatalf("8: %+v %v %v", in, ok, err)
	}
	if r, err := s.GroupResult(ctx, 7); err != nil || string(r) != "{}" {
		t.Fatalf("before kept: %s, %v", r, err)
	}
	if g, err := s.OwnedGroup(ctx, 7); err != nil || g != 0 {
		t.Fatalf("owned before: %d, %v", g, err)
	}
	if err := s.RecordGroup(ctx, 7, 5, []byte(`{"state":"ready","group_id":5,"served":2}`)); err != nil {
		t.Fatal(err)
	}
	if r, err := s.GroupResult(ctx, 7); err != nil || !jsonHas(r, `"group_id": 5`) {
		t.Fatalf("recorded: %s, %v", r, err)
	}
	// A refusal later keeps the group onboarding owns.
	if err := s.RecordGroup(ctx, 7, 0, []byte(`{"state":"unavailable"}`)); err != nil {
		t.Fatal(err)
	}
	if g, err := s.OwnedGroup(ctx, 7); err != nil || g != 5 {
		t.Fatalf("owned after a refusal: %d, %v", g, err)
	}
	// An installation suspended meanwhile records nothing.
	if err := s.RecordGroup(ctx, 8, 9, []byte(`{"state":"ready"}`)); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GroupResult(ctx, 8); string(r) != "{}" {
		t.Fatalf("recorded for a suspended installation: %s", r)
	}
	if g, _ := s.OwnedGroup(ctx, 8); g != 0 {
		t.Fatalf("owned for a suspended installation: %d", g)
	}
}

// One keeper at a time holds an installation's group; another
// installation's is free.
func TestLockInstallation(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	unlock, err := s.LockInstallation(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.LockInstallation(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	other()
	got := make(chan func(), 1)
	go func() {
		u, err := s.LockInstallation(ctx, 7)
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("two keepers held one installation")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	(<-got)()
}
