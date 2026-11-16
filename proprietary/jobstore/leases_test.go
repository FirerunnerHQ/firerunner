package jobstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const testTTL = 30 * time.Second

// joined opens the store on an empty database and joins each shard, all of
// one owner.
func joined(t *testing.T, ids ...string) (*TenantStore, string, []Shard) {
	t.Helper()
	s, dsn := openTenantStore(t)
	var shards []Shard
	for _, id := range ids {
		sh := Shard{ID: id, Host: "h-" + id, Owner: "vm-" + id, TenantOwner: "fr"}
		if err := s.Join(context.Background(), sh); err != nil {
			t.Fatal(err)
		}
		shards = append(shards, sh)
	}
	return s, dsn, shards
}

func renew(t *testing.T, s *TenantStore, sh Shard, accounts ...string) []string {
	t.Helper()
	held, err := s.Renew(context.Background(), sh, accounts, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(held)
	return held
}

func held(t *testing.T, s *TenantStore, sh Shard, want []string, accounts ...string) {
	t.Helper()
	if got := renew(t, s, sh, accounts...); !slices.Equal(got, want) {
		t.Fatalf("%s renewing %v holds %v, want %v", sh.ID, accounts, got, want)
	}
}

func exec(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func query[T any](t *testing.T, dsn, sql string, args ...any) T {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var v T
	if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A shard takes the leases nobody holds and renews its own; a lease another
// shard holds is taken only once it has expired, and its epoch then grows.
func TestLeasesTakeOnlyWhatIsFree(t *testing.T) {
	s, dsn, sh := joined(t, "a", "b")
	held(t, s, sh[0], []string{"x", "y"}, "x", "y")
	held(t, s, sh[1], []string{"z"}, "x", "z")
	held(t, s, sh[0], []string{"x", "y"}, "x", "y")

	exec(t, dsn, `UPDATE leases SET expires_at = now() - interval '1 second' WHERE account = 'x'`)
	held(t, s, sh[1], []string{"x", "z"}, "x", "z")
	if e := query[int64](t, dsn, `SELECT epoch FROM leases WHERE account = 'x'`); e != 2 {
		t.Fatalf("epoch %d after a takeover, want 2", e)
	}
	held(t, s, sh[0], []string{"y"}, "x", "y")
	held(t, s, sh[1], []string{"x", "z"}, "x", "z", "z") // an account asked twice
	if e := query[int64](t, dsn, `SELECT epoch FROM leases WHERE account = 'x'`); e != 2 {
		t.Fatalf("epoch %d after renewals by its holder, want 2", e)
	}

	// The lease runs ttl from the database's now, and the heartbeat moves.
	left := query[float64](t, dsn, `SELECT extract(epoch FROM expires_at - now()) FROM leases WHERE account = 'z'`)
	if left < testTTL.Seconds()-5 || left > testTTL.Seconds() {
		t.Fatalf("lease expires in %.1fs, want about %s", left, testTTL)
	}
	exec(t, dsn, `UPDATE shards SET heartbeat_at = now() - interval '1 hour'`)
	renew(t, s, sh[1], "z")
	if age := query[float64](t, dsn, `SELECT extract(epoch FROM now() - heartbeat_at) FROM shards WHERE id = 'b'`); age > 5 {
		t.Fatalf("heartbeat %.0fs old after a renewal", age)
	}
}

// A shard's leases on accounts it no longer asks for are released in the
// same renewal: another shard takes them at its next.
func TestLeasesReleaseWhatIsNoLongerAskedFor(t *testing.T) {
	s, _, sh := joined(t, "a", "b")
	held(t, s, sh[0], []string{"x", "y"}, "x", "y")
	held(t, s, sh[0], []string{"y"}, "y")
	held(t, s, sh[1], []string{"x"}, "x", "y")
	held(t, s, sh[0], nil)
	held(t, s, sh[1], []string{"x", "y"}, "x", "y")
}

// Release frees every lease of a shard at once and unregisters it.
func TestLeasesRelease(t *testing.T) {
	s, dsn, sh := joined(t, "a", "b")
	held(t, s, sh[0], []string{"x", "y"}, "x", "y")
	if err := s.Release(context.Background(), sh[0]); err != nil {
		t.Fatal(err)
	}
	held(t, s, sh[1], []string{"x", "y"}, "x", "y")
	if n := query[int64](t, dsn, `SELECT count(*) FROM shards WHERE id = 'a'`); n != 0 {
		t.Fatalf("%d rows for a released shard", n)
	}
	// Releasing again takes nothing from the shard that holds them now.
	if err := s.Release(context.Background(), sh[0]); err != nil {
		t.Fatal(err)
	}
	held(t, s, sh[1], []string{"x", "y"}, "x", "y")
}

// A shard may not join beside a live one of another tenants' owner (it
// would leave the runners handed to it running), nor of its own owner (the
// two would end each other's instances). Once the others stop beating, it
// may. A shard joining again, as itself, may.
func TestJoinRefusesOwnersThatDoNotFit(t *testing.T) {
	s, dsn, sh := joined(t, "a")
	ctx := context.Background()
	if err := s.Join(ctx, Shard{ID: "b", Host: "h-b", Owner: "vm-b", TenantOwner: "fr"}); err != nil {
		t.Fatalf("a shard of its own owner and the tenants' owner: %v", err)
	}
	if err := s.Join(ctx, sh[0]); err != nil {
		t.Fatalf("joining again: %v", err)
	}
	for _, c := range []struct {
		sh   Shard
		want string
	}{
		{Shard{ID: "c", Host: "h-c", Owner: "vm-c", TenantOwner: "other"}, "--tenant-owner fr, not other"},
		{Shard{ID: "c", Host: "h-c", Owner: "vm-a", TenantOwner: "fr"}, "live shard a has --owner vm-a too"},
	} {
		err := s.Join(ctx, c.sh)
		if !errors.Is(err, ErrOwnerDiffers) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("Join of %+v = %v, want ErrOwnerDiffers saying %q", c.sh, err, c.want)
		}
	}
	if n := query[int64](t, dsn, `SELECT count(*) FROM shards WHERE id = 'c'`); n != 0 {
		t.Fatal("a refused shard was registered")
	}
	exec(t, dsn, `UPDATE shards SET heartbeat_at = now() - interval '2 minutes'`)
	if err := s.Join(ctx, Shard{ID: "c", Host: "h-c", Owner: "vm-a", TenantOwner: "other"}); err != nil {
		t.Fatalf("Join once the others stopped beating: %v", err)
	}
}

// A process started again on its host, with its owner, replaces the shard
// it was before, as after a crash: it is not refused as a second shard of
// that owner. The earlier one's leases are not handed on: they expire as
// they are, and it renews nothing more.
func TestJoinReplacesThisHostsEarlierShard(t *testing.T) {
	s, dsn, sh := joined(t, "a")
	held(t, s, sh[0], []string{"x"}, "x")
	again := Shard{ID: "a2", Host: sh[0].Host, Owner: sh[0].Owner, TenantOwner: "fr"}
	if err := s.Join(context.Background(), again); err != nil {
		t.Fatalf("joining again after a crash: %v", err)
	}
	if n := query[int64](t, dsn, `SELECT count(*) FROM shards WHERE id = 'a'`); n != 0 {
		t.Fatal("the earlier shard is still registered")
	}
	if _, err := s.Renew(context.Background(), sh[0], []string{"x"}, testTTL); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("the earlier shard's renewal = %v, want ErrNotJoined", err)
	}
	held(t, s, again, nil, "x")
	exec(t, dsn, `UPDATE leases SET expires_at = now() - interval '1 second'`)
	held(t, s, again, []string{"x"}, "x")
}

// Join forgets the shards and leases gone for a day.
func TestJoinForgetsTheLongGone(t *testing.T) {
	s, dsn, sh := joined(t, "a")
	held(t, s, sh[0], []string{"x", "y"}, "x", "y")
	exec(t, dsn, `UPDATE leases SET expires_at = now() - interval '25 hours' WHERE account = 'x'`)
	exec(t, dsn, `UPDATE shards SET heartbeat_at = now() - interval '25 hours'`)
	if err := s.Join(context.Background(), Shard{ID: "b", Host: "h-b", Owner: "vm-b", TenantOwner: "fr"}); err != nil {
		t.Fatal(err)
	}
	if n := query[int64](t, dsn, `SELECT count(*) FROM shards`); n != 1 {
		t.Fatalf("%d shards, want b alone", n)
	}
	if got := query[string](t, dsn, `SELECT string_agg(account, ',') FROM leases`); got != "y" {
		t.Fatalf("leases %q, want y alone", got)
	}
}

// Shards renewing the same accounts at once, each in its own order, never
// share one, and none of them fails: not even when their lists disagree, so
// that each releases leases the others want, which deadlocks a transaction
// now and then.
func TestLeasesUnderContention(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f"}
	s, _, sh := joined(t, ids...)
	var accounts []string
	for i := range 20 {
		accounts = append(accounts, fmt.Sprintf("acct%02d", i))
	}
	for round := range 30 {
		holders := map[string][]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		errs := make(chan error, len(sh))
		for _, x := range sh {
			mine := slices.Clone(accounts)
			rand.Shuffle(len(mine), func(i, j int) { mine[i], mine[j] = mine[j], mine[i] })
			if round >= 5 {
				mine = mine[:len(mine)/2] // the rest released, if held
			}
			wg.Go(func() {
				got, err := s.Renew(context.Background(), x, mine, testTTL)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				for _, a := range got {
					holders[a] = append(holders[a], x.ID)
				}
				mu.Unlock()
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		for _, a := range accounts {
			if len(holders[a]) > 1 || (round < 5 && len(holders[a]) != 1) {
				t.Fatalf("round %d: %s held by %v, want one shard", round, a, holders[a])
			}
		}
	}
}
