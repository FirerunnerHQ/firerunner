package jobstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Shard is one daemon that serves the tenants of the tenants table under
// leases: an id no other process has, the host it runs on, the owner its
// own instances are tagged with, and the tenants' owner, which their
// instances are tagged under (<TenantOwner>/<id>).
type Shard struct {
	ID          string
	Host        string
	Owner       string
	TenantOwner string
}

// ErrNotJoined is a renewal by a shard the database does not have: it was
// forgotten, or replaced by a later process of its host and owner. It must
// join again.
var ErrNotJoined = errors.New("this shard is not registered: join again")

// ErrOwnerDiffers is a shard joining beside a live one whose tenants' owner
// differs, or whose own owner is the same. A shard hands a tenant's busy
// runners to the next by the tenant's owner tag, which the next one's
// reaper ends once their jobs are done: under another, they would be left
// running. And each shard's reaper and pool take every instance of its own
// owner as theirs: two shards of one would end each other's.
var ErrOwnerDiffers = errors.New("the shards' owners do not fit")

// shardLive is how recent a heartbeat a shard that counts as live has. A
// shard beats every 10s.
const shardLive = time.Minute

// shardForgotten is how long a shard that stopped beating, and a lease that
// expired, are kept: long enough to read what happened.
const shardForgotten = 24 * time.Hour

// Join registers sh as a shard. It sets the database up first, and fails
// with ErrOwnerDiffers while a live shard has another tenants' owner, or
// sh's own owner. A shard of sh's host and owner is taken for an earlier
// process of it, as after a crash, and replaced; its leases expire as they
// are. Join also deletes shards and leases gone for a day.
func (s *TenantStore) Join(ctx context.Context, sh Shard) error {
	if err := prepare(ctx, s.pool); err != nil {
		return fmt.Errorf("set up the job store: %w", err)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// One join at a time: two of different owners must not both pass.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7231906)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM shards WHERE host = $1 AND owner = $2 AND id <> $3`, sh.Host, sh.Owner, sh.ID); err != nil {
			return fmt.Errorf("replace this host's earlier shard: %w", err)
		}
		var tenantOwners, same string
		if err := tx.QueryRow(ctx, `SELECT
				coalesce(string_agg(DISTINCT tenant_owner, ', ') FILTER (WHERE tenant_owner <> $2), ''),
				coalesce(string_agg(id, ', ') FILTER (WHERE owner = $1), '')
			FROM shards WHERE id <> $3 AND heartbeat_at > now() - make_interval(secs => $4)`,
			sh.Owner, sh.TenantOwner, sh.ID, shardLive.Seconds()).Scan(&tenantOwners, &same); err != nil {
			return fmt.Errorf("read the shards: %w", err)
		}
		if tenantOwners != "" {
			return fmt.Errorf("%w: live shards have --tenant-owner %s, not %s; every shard needs the same", ErrOwnerDiffers, tenantOwners, sh.TenantOwner)
		}
		if same != "" {
			return fmt.Errorf("%w: live shard %s has --owner %s too; each shard needs its own", ErrOwnerDiffers, same, sh.Owner)
		}
		forgotten := shardForgotten.Seconds()
		if _, err := tx.Exec(ctx, `DELETE FROM shards WHERE heartbeat_at < now() - make_interval(secs => $1)`, forgotten); err != nil {
			return fmt.Errorf("delete the shards gone: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM leases WHERE expires_at < now() - make_interval(secs => $1)`, forgotten); err != nil {
			return fmt.Errorf("delete the leases gone: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shards (id, host, owner, tenant_owner) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO UPDATE SET host = excluded.host, owner = excluded.owner, tenant_owner = excluded.tenant_owner, heartbeat_at = now()`,
			sh.ID, sh.Host, sh.Owner, sh.TenantOwner); err != nil {
			return fmt.Errorf("register the shard: %w", err)
		}
		return nil
	})
}

// Renew beats sh's heartbeat, and takes or renews for ttl the lease of each
// account assigned to sh (Assign) and each in accounts, in one transaction.
// It returns the accounts whose lease sh holds now: a lease another shard
// holds and has not let expire is not taken. sh's leases on accounts it no
// longer asks for are released at once, so another shard may take them.
//
// Every time is the database's. A caller that serves an account only until
// a deadline it sets before calling, shorter than ttl, on its own monotonic
// clock, never serves it once another shard may hold the lease: the
// transaction, and so its now(), begins after that.
//
// Renewals run one at a time: two shards whose lists disagree would lock
// each other's leases in turn, the one releasing what the other takes, and
// deadlock. One takes milliseconds, every few seconds a shard.
//
// A shard the database does not have is ErrNotJoined, and renews nothing.
func (s *TenantStore) Renew(ctx context.Context, sh Shard, accounts []string, ttl time.Duration) ([]string, error) {
	var held []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7231907)`); err != nil {
			return fmt.Errorf("wait for the other renewals: %w", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE shards SET heartbeat_at = now() WHERE id = $1`, sh.ID)
		if err != nil {
			return fmt.Errorf("beat the heartbeat: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotJoined
		}
		rows, err := tx.Query(ctx, `SELECT account FROM assignments WHERE shard = $1`, sh.ID)
		if err != nil {
			return fmt.Errorf("read the assignments: %w", err)
		}
		assigned, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read the assignments: %w", err)
		}
		accounts := append(slices.Clone(accounts), assigned...)
		if _, err := tx.Exec(ctx, `UPDATE leases SET expires_at = now()
			WHERE shard = $1 AND expires_at > now() AND account <> ALL(coalesce($2::text[], '{}'))`, sh.ID, accounts); err != nil {
			return fmt.Errorf("release the leases no longer wanted: %w", err)
		}
		rows, err = tx.Query(ctx, `INSERT INTO leases (account, shard, expires_at)
			SELECT a, $1, now() + make_interval(secs => $3) FROM (SELECT DISTINCT unnest($2::text[]) AS a) wanted ORDER BY a
			ON CONFLICT (account) DO UPDATE SET shard = excluded.shard,
				epoch = leases.epoch + (leases.shard <> excluded.shard)::int,
				expires_at = excluded.expires_at
			WHERE leases.shard = excluded.shard OR leases.expires_at <= now()
			RETURNING account`, sh.ID, accounts, ttl.Seconds())
		if err != nil {
			return fmt.Errorf("renew the leases: %w", err)
		}
		held, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("renew the leases: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return held, nil
}

// Release lets every lease sh holds expire now, and unregisters it: another
// shard may take the accounts at once. Call it once sh serves none of them.
func (s *TenantStore) Release(ctx context.Context, sh Shard) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE leases SET expires_at = now() WHERE shard = $1 AND expires_at > now()`, sh.ID); err != nil {
			return fmt.Errorf("release the leases: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM shards WHERE id = $1`, sh.ID); err != nil {
			return fmt.Errorf("unregister the shard: %w", err)
		}
		return nil
	})
}
