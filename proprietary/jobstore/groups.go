package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReadInstallation returns an installation's row, outside any delivery.
func (s *TenantStore) ReadInstallation(ctx context.Context, id int64) (Installation, bool, error) {
	if err := s.prepareOnce(ctx); err != nil {
		return Installation{}, false, err
	}
	return scanInstallation(s.pool.QueryRow(ctx, `SELECT `+installationColumns+` FROM installations WHERE installation_id = $1`, id))
}

// ActiveInstallations returns the installations onboarding lists a tenant
// for, by id.
func (s *TenantStore) ActiveInstallations(ctx context.Context) ([]Installation, error) {
	if err := s.prepareOnce(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+installationColumns+` FROM installations WHERE state = 'active' ORDER BY installation_id`)
	if err != nil {
		return nil, fmt.Errorf("read the active installations: %w", err)
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		in, _, err := scanInstallation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the active installations: %w", err)
	}
	return out, nil
}

// RecordGroup records what keeping the installation's runner group came
// to, as JSON, and the group onboarding created, if group is not 0. An
// installation no longer active records nothing: its own delivery
// settled it while the group was kept.
func (s *TenantStore) RecordGroup(ctx context.Context, id, group int64, result json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `UPDATE installations SET group_result = $2, group_synced_at = now(),
		group_id = CASE WHEN $3::bigint <> 0 THEN $3::bigint ELSE group_id END
		WHERE installation_id = $1 AND state = 'active'`, id, string(result), group)
	if err != nil {
		return fmt.Errorf("record installation %d's runner group: %w", id, err)
	}
	return nil
}

// OwnedGroup returns the runner group onboarding created for the
// installation's organization, or 0.
func (s *TenantStore) OwnedGroup(ctx context.Context, id int64) (int64, error) {
	var group int64
	err := s.pool.QueryRow(ctx, `SELECT group_id FROM installations WHERE installation_id = $1`, id).Scan(&group)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read installation %d's runner group: %w", id, err)
	}
	return group, nil
}

// LockInstallation holds the installation's runner group for one keeper,
// across processes, until unlock: two at once could each list its
// repositories and set them, the older list last.
func (s *TenantStore) LockInstallation(ctx context.Context, id int64) (unlock func(), err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock installation %d: %w", id, err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('firerunner runner group ' || $1::bigint, 0))`, id); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock installation %d: %w", id, err)
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended('firerunner runner group ' || $1::bigint, 0))`, id); err != nil {
			// A connection that may still hold the lock is never reused.
			_ = conn.Conn().Close(uctx)
		}
		conn.Release()
	}, nil
}

// GroupResult returns what keeping the installation's runner group last
// came to, as recorded: {} until it is kept.
func (s *TenantStore) GroupResult(ctx context.Context, id int64) (json.RawMessage, error) {
	var out string
	if err := s.pool.QueryRow(ctx, `SELECT group_result::text FROM installations WHERE installation_id = $1`, id).Scan(&out); err != nil {
		return nil, fmt.Errorf("read installation %d's runner group: %w", id, err)
	}
	return json.RawMessage(out), nil
}
