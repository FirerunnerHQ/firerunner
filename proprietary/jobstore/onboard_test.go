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

// putTenant is a delivery's fn that lists a tenant for installation 7 and
// records it.
func putTenant(id string) func(OnboardTx) (string, error) {
	return func(tx OnboardTx) (string, error) {
		ctx := context.Background()
		if err := tx.PutTenant(ctx, entry(id, "https://github.com/"+id)); err != nil {
			return "", err
		}
		return "put " + id, tx.PutInstallation(ctx, Installation{ID: 7, AccountLogin: id, AccountType: "Organization",
			Tenant: id, State: "active", UpdatedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)})
	}
}

// A delivery is applied once: the same id again runs nothing, and returns
// the outcome recorded the first time.
func TestOnboardOnce(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	out, err := s.Onboard(ctx, "d1", "installation", 7, putTenant("acme"))
	if err != nil || out != "put acme" {
		t.Fatalf("first: %q, %v", out, err)
	}
	ran := false
	out, err = s.Onboard(ctx, "d1", "installation", 7, func(OnboardTx) (string, error) { ran = true; return "again", nil })
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
	_, err := s.Onboard(ctx, "d1", "installation", 7, func(tx OnboardTx) (string, error) {
		if _, err := putTenant("acme")(tx); err != nil {
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
	if out, err := s.Onboard(ctx, "d1", "installation", 7, putTenant("acme")); err != nil || out != "put acme" {
		t.Fatalf("retry: %q, %v", out, err)
	}
}

// Deliveries of one installation never run beside each other: each reads
// what the one before it wrote.
func TestOnboardSerialisesAnInstallation(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if _, err := s.Onboard(ctx, "seed", "installation", 7, putTenant("acme")); err != nil {
		t.Fatal(err)
	}
	var inside, most atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Onboard(ctx, fmt.Sprintf("d%d", i), "installation", 7, func(tx OnboardTx) (string, error) {
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
		t.Fatalf("%d deliveries of one installation ran at once", most.Load())
	}
	in := readInstallation(t, s, 7)
	if want := time.Date(2026, 11, 18, 0, 0, 8, 0, time.UTC); !in.UpdatedAt.Equal(want) {
		t.Fatalf("updated_at %v, want %v: an update was lost", in.UpdatedAt, want)
	}
}

// A tenant onboarding deletes is a removed tenant, swept like any other;
// once its installation is gone, InstallationGone says so.
func TestOnboardRemovalIsSwept(t *testing.T) {
	s, _ := openTenantStore(t)
	ctx := context.Background()
	if gone, err := s.InstallationGone(ctx, 7); err != nil || gone {
		t.Fatalf("before set-up: %v, %v", gone, err)
	}
	if _, err := s.Onboard(ctx, "d1", "installation", 7, putTenant("acme")); err != nil {
		t.Fatal(err)
	}
	if gone, err := s.InstallationGone(ctx, 7); err != nil || gone {
		t.Fatalf("active: %v, %v", gone, err)
	}
	_, err := s.Onboard(ctx, "d2", "installation", 7, func(tx OnboardTx) (string, error) {
		in, _, err := tx.Installation(ctx, 7)
		if err != nil {
			return "", err
		}
		in.State = "deleted"
		if err := tx.DeleteTenant(ctx, "acme"); err != nil {
			return "", err
		}
		return "removed", tx.PutInstallation(ctx, in)
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := s.RemovedTenants(ctx)
	if err != nil || len(removed) != 1 || removed[0].ID != "acme" {
		t.Fatalf("removed %+v, %v", removed, err)
	}
	if gone, err := s.InstallationGone(ctx, 7); err != nil || !gone {
		t.Fatalf("deleted: %v, %v", gone, err)
	}
	if list, _ := s.Load(ctx); string(list) != "[]" {
		t.Fatalf("tenant acme still listed: %s", list)
	}
	if in := readInstallation(t, s, 7); in.State != "deleted" || in.Tenant != "acme" {
		t.Fatalf("installation %+v", in)
	}
}

var reads atomic.Int32

// readInstallation reads an installation's row, as a delivery would.
func readInstallation(t *testing.T, s *TenantStore, id int64) Installation {
	t.Helper()
	var in Installation
	_, err := s.Onboard(context.Background(), fmt.Sprintf("read-%d", reads.Add(1)), "test", id, func(tx OnboardTx) (string, error) {
		var err error
		in, _, err = tx.Installation(context.Background(), id)
		return "", err
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}
