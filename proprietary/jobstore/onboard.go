package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Installation is one of the hosted App's installations as onboarding has
// applied its webhooks (the installations table).
type Installation struct {
	ID           int64
	AccountID    int64
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
	// LatestInstallation returns the account's installation of the highest
	// id other than except, if it has one: GitHub numbers installations in
	// order, and an account has one at a time.
	LatestInstallation(ctx context.Context, account, except int64) (Installation, bool, error)
	PutInstallation(ctx context.Context, in Installation) error
	// Tenants returns the tenants list's entries, by id.
	Tenants(ctx context.Context) (map[string]json.RawMessage, error)
	PutTenant(ctx context.Context, e Entry) error
	// DeleteTenant deletes the tenant if its entry is still the
	// installation's, and reports whether it did.
	DeleteTenant(ctx context.Context, tenant string, installation int64) (bool, error)
}

// Onboard applies one webhook delivery: fn, in one transaction, which no
// other delivery for the account runs beside, whichever installation it
// names. A delivery whose id was applied before is ErrDelivered, with the
// outcome it had then, and fn is not run. fn returns the outcome to record.
func (s *TenantStore) Onboard(ctx context.Context, delivery, event string, account int64, fn func(OnboardTx) (string, error)) (string, error) {
	if err := s.prepareOnce(ctx); err != nil {
		return "", err
	}
	var outcome string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('firerunner account ' || $1::bigint, 0))`, account); err != nil {
			return fmt.Errorf("wait for the account's other deliveries: %w", err)
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

// ErrOnboarded is a tenants list that leaves out tenants onboarding
// listed for installations still active: written, they would never be
// served again, as nothing re-lists them.
var ErrOnboarded = errors.New("the list leaves out onboarded tenants")

// onboardedLeftOut returns the tenants of active installations that ids
// leaves out.
func onboardedLeftOut(ctx context.Context, tx pgx.Tx, ids []string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT tenant FROM installations WHERE state = 'active' AND tenant <> '' AND tenant <> ALL(coalesce($1, '{}'::text[])) ORDER BY tenant`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the onboarded tenants: %w", err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the onboarded tenants: %w", err)
	}
	return left, nil
}

func onboardedError(left []string) error {
	return fmt.Errorf("%w: %s; their organizations have the App installed, and suspending or uninstalling it there removes them", ErrOnboarded, strings.Join(left, ", "))
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

const installationColumns = `installation_id, account_id, account_login, account_type, tenant, state, reason, updated_at`

func scanInstallation(row pgx.Row) (Installation, bool, error) {
	var in Installation
	err := row.Scan(&in.ID, &in.AccountID, &in.AccountLogin, &in.AccountType, &in.Tenant, &in.State, &in.Reason, &in.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, false, nil
	}
	if err != nil {
		return Installation{}, false, fmt.Errorf("read an installation: %w", err)
	}
	return in, true, nil
}

func (t onboardTx) Installation(ctx context.Context, id int64) (Installation, bool, error) {
	return scanInstallation(t.tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM installations WHERE installation_id = $1`, id))
}

func (t onboardTx) LatestInstallation(ctx context.Context, account, except int64) (Installation, bool, error) {
	return scanInstallation(t.tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM installations
		WHERE account_id = $1 AND installation_id <> $2 ORDER BY installation_id DESC LIMIT 1`, account, except))
}

func (t onboardTx) PutInstallation(ctx context.Context, in Installation) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO installations (`+installationColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (installation_id) DO UPDATE SET account_id = excluded.account_id, account_login = excluded.account_login,
			account_type = excluded.account_type, tenant = excluded.tenant, state = excluded.state, reason = excluded.reason,
			updated_at = excluded.updated_at, applied_at = now()`,
		in.ID, in.AccountID, in.AccountLogin, in.AccountType, in.Tenant, in.State, in.Reason, in.UpdatedAt)
	if err != nil {
		return fmt.Errorf("write installation %d: %w", in.ID, err)
	}
	return nil
}

func (t onboardTx) Tenants(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := t.tx.Query(ctx, `SELECT id, entry::text FROM tenants`)
	if err != nil {
		return nil, fmt.Errorf("read the tenants: %w", err)
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var id, entry string
		if err := rows.Scan(&id, &entry); err != nil {
			return nil, fmt.Errorf("read the tenants: %w", err)
		}
		out[id] = json.RawMessage(entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tenants: %w", err)
	}
	return out, nil
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

func (t onboardTx) DeleteTenant(ctx context.Context, tenant string, installation int64) (bool, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1 AND (entry->>'installation_id')::bigint = $2`, tenant, installation)
	if err != nil {
		return false, fmt.Errorf("delete tenant %q: %w", tenant, err)
	}
	return tag.RowsAffected() > 0, nil
}
