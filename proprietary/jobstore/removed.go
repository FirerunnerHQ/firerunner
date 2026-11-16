package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Removed is a tenant deleted from the tenants table whose leftovers no
// daemon has deleted yet: its last entry, and when it was deleted.
type Removed struct {
	ID        string
	Entry     json.RawMessage
	RemovedAt time.Time
}

// RemovedTenants returns the tenants removed whose leftovers are not
// deleted yet, the oldest first. A database not set up yet has none.
func (s *TenantStore) RemovedTenants(ctx context.Context) ([]Removed, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, entry::text, removed_at FROM removed_tenants ORDER BY removed_at, id`)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
			return nil, nil
		}
		return nil, fmt.Errorf("read the removed tenants: %w", err)
	}
	removed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Removed, error) {
		var t Removed
		var entry string
		err := r.Scan(&t.ID, &entry, &t.RemovedAt)
		t.Entry = json.RawMessage(entry)
		return t, err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return nil, nil
		}
		return nil, fmt.Errorf("read the removed tenants: %w", err)
	}
	return removed, nil
}

// ForgetRemoved records that tenant id's leftovers are deleted. It forgets
// only the removal of removedAt, when that is set: a tenant listed and
// removed again meanwhile is a new removal, still to do.
func (s *TenantStore) ForgetRemoved(ctx context.Context, id string, removedAt time.Time) error {
	var err error
	if removedAt.IsZero() {
		_, err = s.pool.Exec(ctx, `DELETE FROM removed_tenants WHERE id = $1`, id)
	} else {
		_, err = s.pool.Exec(ctx, `DELETE FROM removed_tenants WHERE id = $1 AND removed_at = $2`, id, removedAt)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("forget the removal of tenant %q: %w", id, err)
	}
	return nil
}
