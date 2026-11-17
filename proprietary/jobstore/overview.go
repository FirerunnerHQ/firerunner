package jobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ShardRow is a shard as the shards table has it.
type ShardRow struct {
	ID          string    `json:"id"`
	Host        string    `json:"host"`
	Owner       string    `json:"owner"`
	TenantOwner string    `json:"tenant_owner"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	Draining    bool      `json:"draining"`
	ListOK      bool      `json:"list_ok"`
	// Live is a shard the assigner may place tenants on: it beats, is not
	// draining, and its list applied.
	Live bool `json:"live"`
}

// LeaseRow is a lease as the leases table has it.
type LeaseRow struct {
	Account   string    `json:"account"`
	Shard     string    `json:"shard"`
	Epoch     int64     `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AssignmentRow is an account's assignment.
type AssignmentRow struct {
	Account    string    `json:"account"`
	Shard      string    `json:"shard"`
	Tiers      int       `json:"tiers"`
	AssignedAt time.Time `json:"assigned_at"`
}

// Overview is the shards' state at one moment: what the tables hold, and
// the database's time, which every time in them is.
type Overview struct {
	Now         time.Time
	List        json.RawMessage
	Shards      []ShardRow
	Leases      []LeaseRow
	Assignments []AssignmentRow
	Removed     []Removed
}

// Overview reads every table the shards keep, in one read-only snapshot.
// A table not set up yet reads as empty.
func (s *TenantStore) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var tenants, shards, leases, assignments, removed bool
		if err := tx.QueryRow(ctx, `SELECT now(), to_regclass('tenants') IS NOT NULL, to_regclass('shards') IS NOT NULL,
				to_regclass('leases') IS NOT NULL, to_regclass('assignments') IS NOT NULL, to_regclass('removed_tenants') IS NOT NULL`).
			Scan(&o.Now, &tenants, &shards, &leases, &assignments, &removed); err != nil {
			return fmt.Errorf("read the database: %w", err)
		}
		o.List = json.RawMessage("[]")
		if tenants {
			var list string
			if err := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(entry ORDER BY id), '[]'::jsonb)::text FROM tenants`).Scan(&list); err != nil {
				return fmt.Errorf("read the tenants: %w", err)
			}
			o.List = json.RawMessage(list)
		}
		var err error
		if shards {
			rows, qerr := tx.Query(ctx, `SELECT id, host, owner, tenant_owner, started_at, heartbeat_at, draining, list_ok,
					heartbeat_at > now() - make_interval(secs => $1) AND NOT draining AND list_ok
				FROM shards ORDER BY id`, ShardStale.Seconds())
			if qerr != nil {
				return fmt.Errorf("read the shards: %w", qerr)
			}
			if o.Shards, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ShardRow]); err != nil {
				return fmt.Errorf("read the shards: %w", err)
			}
		}
		if leases {
			rows, qerr := tx.Query(ctx, `SELECT account, shard, epoch, expires_at FROM leases ORDER BY account`)
			if qerr != nil {
				return fmt.Errorf("read the leases: %w", qerr)
			}
			if o.Leases, err = pgx.CollectRows(rows, pgx.RowToStructByPos[LeaseRow]); err != nil {
				return fmt.Errorf("read the leases: %w", err)
			}
		}
		if assignments {
			rows, qerr := tx.Query(ctx, `SELECT account, shard, tiers, assigned_at FROM assignments ORDER BY account`)
			if qerr != nil {
				return fmt.Errorf("read the assignments: %w", qerr)
			}
			if o.Assignments, err = pgx.CollectRows(rows, pgx.RowToStructByPos[AssignmentRow]); err != nil {
				return fmt.Errorf("read the assignments: %w", err)
			}
		}
		if removed {
			rows, qerr := tx.Query(ctx, `SELECT id, entry::text, removed_at FROM removed_tenants ORDER BY removed_at, id`)
			if qerr != nil {
				return fmt.Errorf("read the removed tenants: %w", qerr)
			}
			if o.Removed, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Removed, error) {
				var t Removed
				var entry string
				err := r.Scan(&t.ID, &entry, &t.RemovedAt)
				t.Entry = json.RawMessage(entry)
				return t, err
			}); err != nil {
				return fmt.Errorf("read the removed tenants: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Overview{}, err
	}
	return o, nil
}
