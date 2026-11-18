package jobstore

import (
	"context"
	"encoding/json"
	"fmt"
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
// to, as JSON.
func (s *TenantStore) RecordGroup(ctx context.Context, id int64, result json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, `UPDATE installations SET group_result = $2, group_synced_at = now() WHERE installation_id = $1`, id, string(result))
	if err != nil {
		return fmt.Errorf("record installation %d's runner group: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record installation %d's runner group: no such installation", id)
	}
	return nil
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
