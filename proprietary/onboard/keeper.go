package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// GroupStore is where installations are read and their runner groups'
// results recorded; a *jobstore.TenantStore.
type GroupStore interface {
	ReadInstallation(ctx context.Context, id int64) (jobstore.Installation, bool, error)
	ActiveInstallations(ctx context.Context) ([]jobstore.Installation, error)
	RecordGroup(ctx context.Context, id int64, result json.RawMessage) error
}

// Keeper keeps the runner group of each installation onboarding lists a
// tenant for: after a delivery that may change it (Outcome.KeepGroup),
// and every so often for all of them, as a webhook can be lost.
type Keeper struct {
	Store  GroupStore
	GitHub Groups
	// Lookup reads the installation from GitHub, for its organization's
	// login now: a renamed organization's old one may name another.
	Lookup func(ctx context.Context, installation int64) (Observed, error)
	// Group is the runner group's name: the tenants' runner group.
	Group string
}

// Keep keeps one installation's runner group, if it is active, and records
// what that came to. It reports whether it kept it; an error is one to
// retry.
func (k Keeper) Keep(ctx context.Context, installation int64) (GroupResult, bool, error) {
	if k.Group == "" {
		return GroupResult{}, false, errors.New("onboarding keeps a runner group of a name: none is set")
	}
	in, ok, err := k.Store.ReadInstallation(ctx, installation)
	if err != nil || !ok || in.State != "active" {
		return GroupResult{}, false, err
	}
	org := in.AccountLogin
	if k.Lookup != nil {
		o, err := k.Lookup(ctx, installation)
		if err != nil {
			return GroupResult{}, false, fmt.Errorf("read installation %d from GitHub: %w", installation, err)
		}
		if !o.Found || o.Suspended || o.AccountID != in.AccountID {
			// Its own delivery settles the installation; nothing to keep.
			return GroupResult{}, false, nil
		}
		org = o.Login
	}
	res, err := SyncGroup(ctx, k.GitHub, installation, org, k.Group)
	if err != nil {
		return GroupResult{}, false, fmt.Errorf("installation %d (%s): %w", installation, org, err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return GroupResult{}, false, err
	}
	if err := k.Store.RecordGroup(ctx, installation, b); err != nil {
		return GroupResult{}, false, err
	}
	return res, true, nil
}

// KeepAll keeps every active installation's runner group, going on past
// one that fails.
func (k Keeper) KeepAll(ctx context.Context) error {
	ins, err := k.Store.ActiveInstallations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range ins {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if _, _, err := k.Keep(ctx, in.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
