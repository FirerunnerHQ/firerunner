package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNoEntitlements is a database with no entitlements table yet: no daemon
// or operator has set it up.
var ErrNoEntitlements = errors.New("the job store has no entitlements table yet")

// Entitlement is one tenant's row of the entitlements table: its record as
// billing wrote it, which the caller decodes, and when it was last written.
type Entitlement struct {
	Tenant    string
	Record    json.RawMessage
	UpdatedAt time.Time
}

// Entitlements returns every row, ordered by tenant. A database not set up
// yet is ErrNoEntitlements.
func (s *TenantStore) Entitlements(ctx context.Context) ([]Entitlement, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant, record::text, updated_at FROM entitlements ORDER BY tenant`)
	if err != nil {
		return nil, entitlementsErr(err)
	}
	defer rows.Close()
	var out []Entitlement
	for rows.Next() {
		var e Entitlement
		var record string
		if err := rows.Scan(&e.Tenant, &record, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read the entitlements: %w", err)
		}
		e.Record = json.RawMessage(record)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, entitlementsErr(err)
	}
	return out, nil
}

func entitlementsErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return ErrNoEntitlements
	}
	return fmt.Errorf("read the entitlements: %w", err)
}

// PutEntitlement writes the tenant's record, as an operator or billing
// would. It never sets the database up: whoever did would own every table
// of the job store, and billing's role owning them would stop the daemon's
// writing its own. A database without the table is ErrNoEntitlements; the
// daemon sets it up as it starts.
func (s *TenantStore) PutEntitlement(ctx context.Context, tenant string, record json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO entitlements (tenant, record) VALUES ($1, $2)
		ON CONFLICT (tenant) DO UPDATE SET record = excluded.record`, tenant, string(record))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return fmt.Errorf("write the entitlement of %q: %w: start the daemon on this database first, which sets it up (or check the role's search_path)", tenant, ErrNoEntitlements)
	}
	if err != nil {
		return fmt.Errorf("write the entitlement of %q: %w", tenant, err)
	}
	return nil
}

// DeleteEntitlement removes the tenant's record; one already gone, or a
// database not set up yet, is no error.
func (s *TenantStore) DeleteEntitlement(ctx context.Context, tenant string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM entitlements WHERE tenant = $1`, tenant)
	if err != nil && !errors.Is(entitlementsErr(err), ErrNoEntitlements) {
		return fmt.Errorf("delete the entitlement of %q: %w", tenant, err)
	}
	return nil
}
