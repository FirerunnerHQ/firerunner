package jobstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Installation is one of the hosted App's installations as onboarding has
// applied its webhooks (the installations table).
type Installation struct {
	ID           int64
	AccountLogin string
	AccountType  string
	// Tenant is the tenants list's id it was given, or "".
	Tenant string
	// State is active, suspended, deleted or refused.
	State  string
	Reason string
	// UpdatedAt is the installation's own updated_at, from the last event
	// applied.
	UpdatedAt time.Time
}

// ErrDelivered is a webhook delivery applied before: it changes nothing.
var ErrDelivered = errors.New("webhook delivery applied already")

// deliveriesKept is how long a delivery's id is kept to tell it again.
const deliveriesKept = 7 * 24 * time.Hour

// OnboardTx is what one delivery may read and change, in its transaction.
type OnboardTx interface {
	// Installation returns the installation's row, if it has one.
	Installation(ctx context.Context, id int64) (Installation, bool, error)
	PutInstallation(ctx context.Context, in Installation) error
	// TenantInstallation returns the installation of the tenants list's
	// entry of that id, if there is one.
	TenantInstallation(ctx context.Context, tenant string) (int64, bool, error)
	PutTenant(ctx context.Context, e Entry) error
	DeleteTenant(ctx context.Context, tenant string) error
}

// Onboard applies one webhook delivery: fn, in one transaction, which no
// other delivery of the installation runs beside. A delivery whose id was
// applied before is ErrDelivered, with the outcome it had then, and fn is
// not run. fn returns the outcome to record.
func (s *TenantStore) Onboard(ctx context.Context, delivery, event string, installation int64, fn func(OnboardTx) (string, error)) (string, error) {
	if err := s.prepareOnce(ctx); err != nil {
		return "", err
	}
	var outcome string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('firerunner installation ' || $1::bigint, 0))`, installation); err != nil {
			return fmt.Errorf("wait for the installation's other deliveries: %w", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (delivery_id, event, outcome) VALUES ($1, $2, '')
			ON CONFLICT (delivery_id) DO NOTHING`, delivery, event)
		if err != nil {
			return fmt.Errorf("record the delivery: %w", err)
		}
		if tag.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `SELECT outcome FROM webhook_deliveries WHERE delivery_id = $1`, delivery).Scan(&outcome); err != nil {
				return fmt.Errorf("read the delivery: %w", err)
			}
			return ErrDelivered
		}
		if outcome, err = fn(onboardTx{tx}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET outcome = $2 WHERE delivery_id = $1`, delivery, outcome); err != nil {
			return fmt.Errorf("record the outcome: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM webhook_deliveries WHERE received_at < now() - make_interval(secs => $1)`, deliveriesKept.Seconds()); err != nil {
			return fmt.Errorf("forget old deliveries: %w", err)
		}
		return nil
	})
	return outcome, err
}

// InstallationGone reports whether onboarding found the installation
// deleted or suspended: its tokens no longer work, so what it registered
// at GitHub cannot be deleted, and need not be swept again.
func (s *TenantStore) InstallationGone(ctx context.Context, installation int64) (bool, error) {
	var gone bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM installations WHERE installation_id = $1 AND state IN ('deleted', 'suspended'))`, installation).Scan(&gone)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // no onboarding yet
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read installation %d: %w", installation, err)
	}
	return gone, nil
}

// prepareOnce sets the database up, once a process.
func (s *TenantStore) prepareOnce(ctx context.Context) error {
	if s.prepared.Load() {
		return nil
	}
	if err := prepare(ctx, s.pool); err != nil {
		return fmt.Errorf("set up the job store: %w", err)
	}
	s.prepared.Store(true)
	return nil
}

type onboardTx struct{ tx pgx.Tx }

func (t onboardTx) Installation(ctx context.Context, id int64) (Installation, bool, error) {
	in := Installation{ID: id}
	err := t.tx.QueryRow(ctx, `SELECT account_login, account_type, tenant, state, reason, updated_at FROM installations WHERE installation_id = $1`, id).
		Scan(&in.AccountLogin, &in.AccountType, &in.Tenant, &in.State, &in.Reason, &in.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, false, nil
	}
	if err != nil {
		return Installation{}, false, fmt.Errorf("read installation %d: %w", id, err)
	}
	return in, true, nil
}

func (t onboardTx) PutInstallation(ctx context.Context, in Installation) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO installations (installation_id, account_login, account_type, tenant, state, reason, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (installation_id) DO UPDATE SET account_login = excluded.account_login, account_type = excluded.account_type,
			tenant = excluded.tenant, state = excluded.state, reason = excluded.reason, updated_at = excluded.updated_at, applied_at = now()`,
		in.ID, in.AccountLogin, in.AccountType, in.Tenant, in.State, in.Reason, in.UpdatedAt)
	if err != nil {
		return fmt.Errorf("write installation %d: %w", in.ID, err)
	}
	return nil
}

func (t onboardTx) TenantInstallation(ctx context.Context, tenant string) (int64, bool, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `SELECT coalesce((entry->>'installation_id')::bigint, 0) FROM tenants WHERE id = $1`, tenant).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read tenant %q: %w", tenant, err)
	}
	return id, true, nil
}

func (t onboardTx) PutTenant(ctx context.Context, e Entry) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO tenants (id, entry) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET entry = excluded.entry, updated_at = now()
		WHERE tenants.entry IS DISTINCT FROM excluded.entry`, e.ID, string(e.Entry))
	if err != nil {
		return fmt.Errorf("write tenant %q: %w", e.ID, err)
	}
	return nil
}

func (t onboardTx) DeleteTenant(ctx context.Context, tenant string) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant); err != nil {
		return fmt.Errorf("delete tenant %q: %w", tenant, err)
	}
	return nil
}
