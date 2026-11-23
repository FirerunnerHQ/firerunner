package jobstore

import (
	"context"
	"fmt"
	"strings"
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
	// Its account's group (installation 7's, the same account), not the 9 it
	// tried to record.
	if g, _ := s.OwnedGroup(ctx, 8); g != 5 {
		t.Fatalf("owned for a suspended installation: %d, want its account's 5", g)
	}
}

// Keeping a group sets the runner group the tenant's scale sets are
// registered in: its id once ready, 0 while it is not (refused, as when
// the organization has a group of that name of its own), which serves the
// tenant nothing. The entry is rewritten only when that changes, as a
// rewrite restarts the tenant; another tenant's is never touched.
func TestRecordGroupSetsTheTenantsGroup(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	for _, id := range []int64{7, 8} {
		if _, err := s.Onboard(ctx, fmt.Sprint("d", id), "installation", putTenant(fmt.Sprint("t", id), id)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(id string) (string, time.Time) {
		t.Helper()
		var group string
		var at time.Time
		if err := s.pool.QueryRow(ctx, `SELECT coalesce(entry->>'runner_group_id', 'absent'), updated_at FROM tenants WHERE id = $1`, id).Scan(&group, &at); err != nil {
			t.Fatal(err)
		}
		return group, at
	}
	if g, _ := entry("t7"); g != "absent" {
		t.Fatalf("before any keep: %s", g)
	}
	if err := s.RecordGroup(ctx, 7, 0, []byte(`{"state":"unavailable","reason":"a group of that name exists"}`)); err != nil {
		t.Fatal(err)
	}
	if g, _ := entry("t7"); g != "0" {
		t.Fatalf("refused: runner_group_id %s, want 0", g)
	}
	if err := s.RecordGroup(ctx, 7, 42, []byte(`{"state":"ready","group_id":42}`)); err != nil {
		t.Fatal(err)
	}
	g, at := entry("t7")
	if g != "42" {
		t.Fatalf("ready: runner_group_id %s, want 42", g)
	}
	if err := s.RecordGroup(ctx, 7, 42, []byte(`{"state":"ready","group_id":42,"served":3}`)); err != nil {
		t.Fatal(err)
	}
	if g2, at2 := entry("t7"); g2 != "42" || !at2.Equal(at) {
		t.Fatalf("kept again: %s at %v, want 42 unchanged since %v", g2, at2, at)
	}
	if g, _ := entry("t8"); g != "absent" {
		t.Fatalf("another tenant's entry: %s", g)
	}
	// A group refused after it was ready serves the tenant nothing again,
	// though onboarding still owns it, and the refusal names it.
	if err := s.RecordGroup(ctx, 7, 42, []byte(`{"state":"unavailable","group_id":42}`)); err != nil {
		t.Fatal(err)
	}
	if g, _ := entry("t7"); g != "0" {
		t.Fatalf("refused after ready: %s, want 0", g)
	}
	if g, err := s.OwnedGroup(ctx, 7); err != nil || g != 42 {
		t.Fatalf("owned after a refusal: %d, %v", g, err)
	}
}

// An import never sets an onboarded tenant's runner group: it keeps the
// one onboarding stored, or 0 when none is, whatever the file says or
// leaves out. A tenant an operator listed keeps the group its entry names.
func TestReplaceKeepsTheOnboardedGroup(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	for _, id := range []int64{7, 8} {
		if _, err := s.Onboard(ctx, fmt.Sprint("d", id), "installation", putTenant(fmt.Sprint("t", id), id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordGroup(ctx, 7, 42, []byte(`{"state":"ready","group_id":42}`)); err != nil {
		t.Fatal(err)
	}
	group := func(id string) string {
		t.Helper()
		var g string
		if err := s.pool.QueryRow(ctx, `SELECT coalesce(entry->>'runner_group_id', 'absent') FROM tenants WHERE id = $1`, id).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	with := func(e Entry, group string) Entry {
		e.Entry = []byte(strings.Replace(string(e.Entry), `"tiers"`, `"runner_group_id":`+group+`,"tiers"`, 1))
		return e
	}
	op := Entry{ID: "op", Entry: []byte(`{"id":"op","url":"https://github.com/op","installation_id":9,"tiers":["small"],"runner_group_id":5}`)}
	// t7's file names another group; t8's (never kept) names one; neither counts.
	if err := s.Replace(ctx, []Entry{with(entryOf("t7", 7), "3"), with(entryOf("t8", 8), "9"), op}); err != nil {
		t.Fatal(err)
	}
	if g := group("t7"); g != "42" {
		t.Errorf("t7 after an import naming 3: %s, want 42", g)
	}
	if g := group("t8"); g != "0" {
		t.Errorf("t8 after an import naming 9: %s, want 0, not ready", g)
	}
	if g := group("op"); g != "5" {
		t.Errorf("an operator's tenant: %s, want its own 5", g)
	}
	// An older file, with no group at all.
	if err := s.Replace(ctx, []Entry{entryOf("t7", 7), entryOf("t8", 8), op}); err != nil {
		t.Fatal(err)
	}
	if g := group("t7"); g != "42" {
		t.Errorf("t7 after an import with none: %s, want 42", g)
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

// An organization that uninstalls and installs again keeps the group
// onboarding created under its first installation; another organization's
// is never its own.
func TestOwnedGroupAcrossReinstalls(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	put := func(id, account int64, state string) {
		t.Helper()
		_, err := s.Onboard(ctx, fmt.Sprintf("d%d-%s", id, state), "installation", func(tx OnboardTx) (string, error) {
			return "", tx.PutInstallation(ctx, Installation{ID: id, AccountID: account, AccountLogin: "org", AccountType: "Organization",
				Tenant: "org", State: state, UpdatedAt: day})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	put(10, 900, "active")
	if err := s.RecordGroup(ctx, 10, 3, []byte(`{"state":"ready","group_id":3}`)); err != nil {
		t.Fatal(err)
	}
	put(10, 900, "deleted")
	put(11, 900, "active")
	if g, err := s.OwnedGroup(ctx, 11); err != nil || g != 3 {
		t.Fatalf("after a reinstall: %d, %v; want the first installation's group 3", g, err)
	}
	put(12, 901, "active")
	if g, err := s.OwnedGroup(ctx, 12); err != nil || g != 0 {
		t.Fatalf("another organization: %d, %v", g, err)
	}
	// Created again under the new installation (the old one deleted at
	// GitHub): the newer group is the one kept.
	if err := s.RecordGroup(ctx, 11, 7, []byte(`{"state":"ready","group_id":7}`)); err != nil {
		t.Fatal(err)
	}
	if g, err := s.OwnedGroup(ctx, 11); err != nil || g != 7 {
		t.Fatalf("after a new group: %d, %v; want 7", g, err)
	}
	if g, err := s.OwnedGroup(ctx, 99); err != nil || g != 0 {
		t.Fatalf("no such installation: %d, %v", g, err)
	}
}
