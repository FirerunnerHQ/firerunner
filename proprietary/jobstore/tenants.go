package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoTenants is a database with no tenants table yet: no daemon or operator
// has set it up.
var ErrNoTenants = errors.New("the job store has no tenants table yet")

// TenantStore keeps the tenants list in the job store's database, one row a
// tenant, so that a list of any length and, later, per-tenant leases live in
// one place. It holds each entry as written; checking one is the caller's.
type TenantStore struct {
	pool *pgxpool.Pool
	// prepared is set once this process has set the database up.
	prepared atomic.Bool
}

// OpenTenants connects to the store's database. It fails only on a URL that
// can never work, without echoing the URL; a database that is down fails the
// read or write.
func OpenTenants(dsn string) (*TenantStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("job store: the database URL is not valid")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, errors.New("job store: the database URL is not valid")
	}
	return &TenantStore{pool: pool}, nil
}

// Close closes the connections.
func (s *TenantStore) Close() { s.pool.Close() }

// Load returns the list as a JSON array of the entries, ordered by id: what a
// tenants list file holds. A database not set up yet is ErrNoTenants.
func (s *TenantStore) Load(ctx context.Context) ([]byte, error) {
	var list string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(entry ORDER BY id), '[]'::jsonb)::text FROM tenants`).Scan(&list)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return nil, ErrNoTenants
	}
	if err != nil {
		return nil, fmt.Errorf("read the tenants: %w", err)
	}
	return []byte(list), nil
}

// Entry is one tenant as Replace writes it: its id and its entry's JSON,
// whose "id" must be the same.
type Entry struct {
	ID    string
	Entry json.RawMessage
}

// Replace makes the stored list entries, in one transaction: a tenant left
// out is deleted, and only an entry that changed is rewritten, so updated_at
// says when each last did. It sets the database up first, as the daemon's
// writer does. A list that leaves out a tenant onboarding listed for an
// installation still active, or changes one onboarding gave an
// installation, is ErrOnboarded, and writes nothing.
func (s *TenantStore) Replace(ctx context.Context, entries []Entry) error {
	if err := prepare(ctx, s.pool); err != nil {
		return fmt.Errorf("set up the job store: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// No webhook applies beside it: one could list a tenant this
		// would then delete.
		if _, err := tx.Exec(ctx, `LOCK TABLE installations IN EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("wait for webhooks being applied: %w", err)
		}
		left, err := onboardedLeftOut(ctx, tx, ids)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return onboardedError(left)
		}
		bad, err := onboardedRewritten(ctx, tx, entries)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			return fmt.Errorf("%w: %s; onboarding lists those for their organizations' installations", ErrOnboarded, strings.Join(bad, ", "))
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id <> ALL($1)`, ids); err != nil {
			return fmt.Errorf("delete the tenants left out: %w", err)
		}
		for _, e := range entries {
			if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, entry) VALUES ($1, $2)
				ON CONFLICT (id) DO UPDATE SET entry = excluded.entry, updated_at = now()
				WHERE tenants.entry IS DISTINCT FROM excluded.entry`, e.ID, string(e.Entry)); err != nil {
				return fmt.Errorf("write tenant %q: %w", e.ID, err)
			}
		}
		return nil
	})
}
