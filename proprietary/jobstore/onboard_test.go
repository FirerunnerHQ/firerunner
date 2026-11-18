package jobstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var day = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

// putTenant is a delivery's fn that lists tenant id for installation inst
// of account 900, and records it.
func putTenant(id string, inst int64) func(OnboardTx) (string, error) {
	return func(tx OnboardTx) (string, error) {
		ctx := context.Background()
		if err := tx.PutTenant(ctx, entryOf(id, inst)); err != nil {
			return "", err
		}
		return "put " + id, tx.PutInstallation(ctx, Installation{ID: inst, AccountID: 900, AccountLogin: id, AccountType: "Organization",
			Tenant: id, State: "active", UpdatedAt: day})
	}
}

func entryOf(id string, inst int64) Entry {
	return Entry{ID: id, Entry: []byte(fmt.Sprintf(`{"id":%q,"url":"https://github.com/%s","installation_id":%d,"tiers":["small"]}`, id, id, inst))}
}

// A delivery is applied once: the same id again runs nothing, and returns
// the outcome recorded the first time.
func TestOnboardOnce(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	out, err := s.Onboard(ctx, "d1", "installation", 900, putTenant("acme", 7))
	if err != nil || out != "put acme" {
		t.Fatalf("first: %q, %v", out, err)
	}
	ran := false
	out, err = s.Onboard(ctx, "d1", "installation", 900, func(OnboardTx) (string, error) { ran = true; return "again", nil })
	if !errors.Is(err, ErrDelivered) || out != "put acme" || ran {
		t.Fatalf("again: %q, %v, ran %v", out, err, ran)
	}
	list, err := s.Load(ctx)
	if err != nil || !jsonHas(list, `"id": "acme"`) {
		t.Fatalf("list %s, %v", list, err)
	}
}

// A delivery that fails applies nothing, and is not recorded: its retry is
// applied in full.
func TestOnboardFailureRollsBack(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	_, err := s.Onboard(ctx, "d1", "installation", 900, func(tx OnboardTx) (string, error) {
		if _, err := putTenant("acme", 7)(tx); err != nil {
			return "", err
		}
		return "", errors.New("later step failed")
	})
	if err == nil {
		t.Fatal("no error")
	}
	if list, _ := s.Load(ctx); string(list) != "[]" {
		t.Fatalf("a failed delivery left %s", list)
	}
	if out, err := s.Onboard(ctx, "d1", "installation", 900, putTenant("acme", 7)); err != nil || out != "put acme" {
		t.Fatalf("retry: %q, %v", out, err)
	}
}

// Deliveries for one account never run beside each other: each reads what
// the one before it wrote.
func TestOnboardSerialisesAnAccount(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if _, err := s.Onboard(ctx, "seed", "installation", 900, putTenant("acme", 7)); err != nil {
		t.Fatal(err)
	}
	var inside, most atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Onboard(ctx, fmt.Sprintf("d%d", i), "installation", 900, func(tx OnboardTx) (string, error) {
				n := inside.Add(1)
				defer inside.Add(-1)
				for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
				}
				in, ok, err := tx.Installation(ctx, 7)
				if err != nil || !ok {
					return "", errors.New("installation not read")
				}
				// A read-modify-write that loses updates unless serialised.
				time.Sleep(5 * time.Millisecond)
				in.UpdatedAt = in.UpdatedAt.Add(time.Second)
				return "", tx.PutInstallation(ctx, in)
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Fatalf("%d deliveries of one account ran at once", most.Load())
	}
	if in := readInstallation(t, s, 7); !in.UpdatedAt.Equal(day.Add(8 * time.Second)) {
		t.Fatalf("updated_at %v: an update was lost", in.UpdatedAt)
	}
}

// A tenant is deleted only while its entry is the installation's, and the
// deletion is a removed tenant, swept like any other.
func TestOnboardDeleteTenant(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if _, err := s.Onboard(ctx, "d1", "installation", 900, putTenant("acme", 8)); err != nil {
		t.Fatal(err)
	}
	del := func(delivery string, inst int64) bool {
		var deleted bool
		_, err := s.Onboard(ctx, delivery, "installation", 900, func(tx OnboardTx) (string, error) {
			var err error
			deleted, err = tx.DeleteTenant(ctx, "acme", inst)
			return "", err
		})
		if err != nil {
			t.Fatal(err)
		}
		return deleted
	}
	if del("d2", 7) {
		t.Fatal("another installation's delete removed the tenant")
	}
	if list, _ := s.Load(ctx); !jsonHas(list, `"id": "acme"`) {
		t.Fatalf("list %s", list)
	}
	if !del("d3", 8) {
		t.Fatal("its own installation's delete left the tenant")
	}
	removed, err := s.RemovedTenants(ctx)
	if err != nil || len(removed) != 1 || removed[0].ID != "acme" {
		t.Fatalf("removed %+v, %v", removed, err)
	}
}

// The account's latest installation is the one of the highest id.
func TestOnboardLatestInstallation(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	for _, id := range []int64{12, 30, 21} {
		if _, err := s.Onboard(ctx, fmt.Sprint(id), "installation", 900, putTenant("acme", id)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Onboard(ctx, "q", "installation", 900, func(tx OnboardTx) (string, error) {
		if in, ok, err := tx.LatestInstallation(ctx, 900, 0); err != nil || !ok || in.ID != 30 {
			t.Errorf("latest: %+v %v %v", in, ok, err)
		}
		if in, ok, err := tx.LatestInstallation(ctx, 900, 30); err != nil || !ok || in.ID != 21 {
			t.Errorf("latest but 30: %+v %v %v", in, ok, err)
		}
		if _, ok, err := tx.LatestInstallation(ctx, 901, 0); err != nil || ok {
			t.Errorf("another account: %v %v", ok, err)
		}
		ts, err := tx.Tenants(ctx)
		if err != nil || len(ts) != 1 || !jsonHas(ts["acme"], `"installation_id": 21`) {
			t.Errorf("tenants %s, %v", ts["acme"], err)
		}
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An import that leaves out a tenant onboarding listed for an active
// installation writes nothing; once the installation is gone, it may.
func TestReplaceKeepsOnboardedTenants(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if _, err := s.Onboard(ctx, "d1", "installation", 900, putTenant("acme", 7)); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, []Entry{entry("b", "https://github.com/b")}); !errors.Is(err, ErrOnboarded) {
		t.Fatalf("import without acme: %v", err)
	}
	if list, _ := s.Load(ctx); !jsonHas(list, `"id": "acme"`) || jsonHas(list, `"id": "b"`) {
		t.Fatalf("list %s", list)
	}
	if err := s.Replace(ctx, []Entry{entryOf("acme", 7), entry("b", "https://github.com/b")}); err != nil {
		t.Fatalf("import keeping acme: %v", err)
	}
	_, err := s.Onboard(ctx, "d2", "installation", 900, func(tx OnboardTx) (string, error) {
		in, _, err := tx.Installation(ctx, 7)
		in.State = "deleted"
		if err == nil {
			err = tx.PutInstallation(ctx, in)
		}
		return "", err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, nil); err != nil {
		t.Fatalf("import without acme, uninstalled: %v", err)
	}
}

var reads atomic.Int32

// readInstallation reads an installation's row, as a delivery would.
func readInstallation(t *testing.T, s *TenantStore, id int64) Installation {
	t.Helper()
	var in Installation
	_, err := s.Onboard(context.Background(), fmt.Sprintf("read-%d", reads.Add(1)), "test", 900, func(tx OnboardTx) (string, error) {
		var err error
		in, _, err = tx.Installation(context.Background(), id)
		return "", err
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}
