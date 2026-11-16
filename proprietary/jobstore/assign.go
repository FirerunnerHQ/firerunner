package jobstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// AssignerLease is the leases row whose holder is the assigner: the one
// shard that writes the assignments. A shard asks for it with its
// accounts; it is no GitHub account (those hold a host and a /).
const AssignerLease = "!assigner"

// ShardStale is how old a heartbeat a shard the assigner counts as gone
// has: three missed beats.
const ShardStale = 30 * time.Second

// ErrNotAssigner is an Assign by a shard that does not hold the
// assigner's lease (any more): another shard may be assigning.
var ErrNotAssigner = errors.New("this shard does not hold the assigner's lease")

// ShardState is a shard as the assigner sees it. Live is a shard that beats
// and is not draining: one an account may be assigned to.
type ShardState struct {
	ID       string
	Draining bool
	Live     bool
}

// Assignment is the shard an account is assigned to, and how many tiers its
// tenant lists.
type Assignment struct {
	Shard string
	Tiers int
}

// Placement reads the shards, ordered by id, and the assignments by
// account, at one moment.
func (s *TenantStore) Placement(ctx context.Context) ([]ShardState, map[string]Assignment, error) {
	var shards []ShardState
	current := map[string]Assignment{}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, draining, heartbeat_at > now() - make_interval(secs => $1) FROM shards ORDER BY id`, ShardStale.Seconds())
		if err != nil {
			return fmt.Errorf("read the shards: %w", err)
		}
		shards, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ShardState, error) {
			var st ShardState
			var beats bool
			err := r.Scan(&st.ID, &st.Draining, &beats)
			st.Live = beats && !st.Draining
			return st, err
		})
		if err != nil {
			return fmt.Errorf("read the shards: %w", err)
		}
		rows, err = tx.Query(ctx, `SELECT account, shard, tiers FROM assignments`)
		if err != nil {
			return fmt.Errorf("read the assignments: %w", err)
		}
		var account string
		var a Assignment
		_, err = pgx.ForEachRow(rows, []any{&account, &a.Shard, &a.Tiers}, func() error {
			current[account] = a
			return nil
		})
		if err != nil {
			return fmt.Errorf("read the assignments: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return shards, current, nil
}

// Assign makes the assignments next, if sh holds the assigner's lease:
// else ErrNotAssigner, and nothing changes. An account left out is
// unassigned, and an assignment rewritten only if it changed, so
// assigned_at says when the account last moved.
func (s *TenantStore) Assign(ctx context.Context, sh Shard, next map[string]Assignment) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Locked to the end: the lease cannot pass to another meanwhile.
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leases
			WHERE account = $1 AND shard = $2 AND expires_at > now() FOR UPDATE)`, AssignerLease, sh.ID).Scan(&held); err != nil {
			return fmt.Errorf("check the assigner's lease: %w", err)
		}
		if !held {
			return ErrNotAssigner
		}
		accounts := slices.Sorted(maps.Keys(next))
		if _, err := tx.Exec(ctx, `DELETE FROM assignments WHERE account <> ALL(coalesce($1::text[], '{}'))`, accounts); err != nil {
			return fmt.Errorf("unassign: %w", err)
		}
		for _, account := range accounts {
			a := next[account]
			if _, err := tx.Exec(ctx, `INSERT INTO assignments (account, shard, tiers) VALUES ($1, $2, $3)
				ON CONFLICT (account) DO UPDATE SET shard = excluded.shard, tiers = excluded.tiers,
					assigned_at = CASE WHEN assignments.shard = excluded.shard THEN assignments.assigned_at ELSE now() END
				WHERE (assignments.shard, assignments.tiers) IS DISTINCT FROM (excluded.shard, excluded.tiers)`,
				account, a.Shard, a.Tiers); err != nil {
				return fmt.Errorf("assign %s: %w", account, err)
			}
		}
		return nil
	})
}

// Drain marks sh as stopping: the assigner moves its accounts to the other
// shards at once, rather than once its heartbeat is stale.
func (s *TenantStore) Drain(ctx context.Context, sh Shard) error {
	if _, err := s.pool.Exec(ctx, `UPDATE shards SET draining = true WHERE id = $1`, sh.ID); err != nil {
		return fmt.Errorf("mark the shard draining: %w", err)
	}
	return nil
}
