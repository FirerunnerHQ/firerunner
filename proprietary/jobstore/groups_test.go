package jobstore

import (
	"context"
	"testing"
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
	if err := s.RecordGroup(ctx, 7, []byte(`{"state":"ready","group_id":5,"served":2}`)); err != nil {
		t.Fatal(err)
	}
	if r, err := s.GroupResult(ctx, 7); err != nil || !jsonHas(r, `"group_id": 5`) {
		t.Fatalf("recorded: %s, %v", r, err)
	}
	if err := s.RecordGroup(ctx, 99, []byte(`{}`)); err == nil {
		t.Fatal("recorded for an installation that does not exist")
	}
}
