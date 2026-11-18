// Package onboard turns the hosted GitHub App's webhooks into the tenants
// list: an organization that installs the App becomes a tenant, served by
// the shards; one that suspends or uninstalls it stops being one (#170).
//
// A webhook is a trigger, not the truth: GitHub delivers each at least
// once, in any order, and does not retry a failed one by itself. So a
// delivery is verified, applied once, under a lock on its account (an
// organization has one installation of the App at a time, and keeps its
// tenant id across reinstalls), and, with Config.Lookup, applies the
// installation's state as GitHub reports it then, not as the event says.
// Without Lookup an event older than the one applied for its installation
// changes nothing.
//
// An organization's rename reaches the tenant's URL at the installation's
// next event: organization.renamed is not one.
package onboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// Config is how deliveries are checked and what a new tenant gets.
type Config struct {
	// Secret is the App's webhook secret, which signs every delivery.
	Secret []byte
	// Tiers are the catalog tiers every new tenant gets.
	Tiers []string
	// RunnerGroup is the runner group its scale sets go in; "" is the
	// daemon's --runner-group.
	RunnerGroup string
	// Reserved are accounts (config.AccountOf) served otherwise, such as the
	// install's own --url: an installation there gets no tenant.
	Reserved []string
	// Check, when set, checks the tenants list a delivery would leave, as
	// the daemon checks each list it reads ((*config.Config).CheckTenants):
	// one it refuses is never written, and the installation is refused.
	Check func([]config.Tenant) error
	// Lookup, when set, reads the installation from GitHub, whose state a
	// delivery then applies.
	Lookup func(ctx context.Context, installation int64) (Observed, error)
}

// Observed is an installation as GitHub reports it.
type Observed struct {
	// Found is false for an installation GitHub no longer has.
	Found     bool
	Suspended bool
	AccountID int64
	Login     string
	// Type is the account's: Organization, User or Enterprise.
	Type string
}

// Validate checks cfg against the daemon's configuration: a new tenant's
// entry must be one the daemon serves, or every delivery would be refused.
func (cfg Config) Validate(daemon *config.Config) error {
	if len(cfg.Secret) == 0 {
		return errors.New("onboarding needs the App's webhook secret")
	}
	if len(cfg.Tiers) == 0 {
		return errors.New("onboarding needs the tiers a new tenant gets")
	}
	probe := config.Tenant{ID: "onboarding-probe", URL: "https://github.com/onboarding-probe", InstallationID: 1,
		Tiers: cfg.Tiers, RunnerGroup: cfg.RunnerGroup}
	if err := daemon.CheckTenants([]config.Tenant{probe}); err != nil {
		return fmt.Errorf("a new tenant's entry would not be served: %w", err)
	}
	return nil
}

// Delivery is one webhook delivery as GitHub sent it.
type Delivery struct {
	// ID is X-GitHub-Delivery, Event X-GitHub-Event, and Signature
	// X-Hub-Signature-256.
	ID, Event, Signature string
	Body                 []byte
}

// Outcome is what applying a delivery did.
type Outcome struct {
	Delivery     string `json:"delivery"`
	Event        string `json:"event"`
	Action       string `json:"action,omitempty"`
	Installation int64  `json:"installation,omitempty"`
	Account      string `json:"account,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	// Result is listed, suspended, removed, refused, stale (older than what
	// was applied), ignored (an event onboarding does not act on), or
	// duplicate (a delivery applied before).
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
}

// Store is where deliveries are applied; a *jobstore.TenantStore.
type Store interface {
	Onboard(ctx context.Context, delivery, event string, account int64, fn func(jobstore.OnboardTx) (string, error)) (string, error)
}

// ErrSignature is a delivery whose signature does not hold: not GitHub's,
// or for another secret.
var ErrSignature = errors.New("the webhook signature does not hold")

// Verify checks a delivery's X-Hub-Signature-256 against secret.
func Verify(secret, body []byte, signature string) error {
	got, ok := strings.CutPrefix(signature, "sha256=")
	if !ok || len(secret) == 0 {
		return ErrSignature
	}
	sum, err := hex.DecodeString(got)
	if err != nil {
		return ErrSignature
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return ErrSignature
	}
	return nil
}

// installationEvent is the part of an installation event onboarding reads.
type installationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"installation"`
}

// Handle verifies a delivery and applies it. A delivery whose signature
// does not hold is ErrSignature, and changes nothing; one that does not
// parse, or fails to apply, is an error, and changes nothing either, so
// it can be retried. Events onboarding does not act on are ignored.
func Handle(ctx context.Context, cfg Config, store Store, d Delivery) (Outcome, error) {
	out := Outcome{Delivery: d.ID, Event: d.Event}
	if err := Verify(cfg.Secret, d.Body, d.Signature); err != nil {
		return out, err
	}
	if d.ID == "" {
		return out, errors.New("the delivery has no X-GitHub-Delivery id")
	}
	if d.Event != "installation" {
		out.Result, out.Reason = "ignored", "onboarding acts on installation events alone"
		return out, nil
	}
	var ev installationEvent
	if err := json.Unmarshal(d.Body, &ev); err != nil {
		return out, fmt.Errorf("installation event: %w", err)
	}
	in := ev.Installation
	if in.ID <= 0 || in.Account.ID <= 0 || in.Account.Login == "" || in.UpdatedAt.IsZero() {
		return out, errors.New("installation event: no installation id, account or updated_at")
	}
	out.Action, out.Installation, out.Account = ev.Action, in.ID, in.Account.Login
	recorded, err := store.Onboard(ctx, d.ID, d.Event, in.Account.ID, func(tx jobstore.OnboardTx) (string, error) {
		o := out
		if err := apply(ctx, cfg, tx, ev, &o); err != nil {
			return "", err
		}
		out = o
		return encode(out), nil
	})
	if errors.Is(err, jobstore.ErrDelivered) {
		return decodeDuplicate(out, recorded), nil
	}
	return out, err
}

// apply moves the installation to the state it is in, and its account's
// tenant with it. The caller holds the account for the transaction.
func apply(ctx context.Context, cfg Config, tx jobstore.OnboardTx, ev installationEvent, out *Outcome) error {
	in := ev.Installation
	login, kind := in.Account.Login, in.Account.Type
	var state string
	if cfg.Lookup != nil {
		o, err := cfg.Lookup(ctx, in.ID)
		if err != nil {
			return fmt.Errorf("read installation %d from GitHub: %w", in.ID, err)
		}
		switch {
		case !o.Found:
			state = "deleted"
		case o.AccountID != in.Account.ID:
			return fmt.Errorf("installation %d: GitHub reports account %d, the event %d", in.ID, o.AccountID, in.Account.ID)
		case o.Suspended:
			state = "suspended"
		default:
			state = "active"
		}
		if o.Found {
			login, kind = o.Login, o.Type
			out.Account = login
		}
	} else {
		var ok bool
		if state, ok = map[string]string{"created": "active", "unsuspend": "active", "suspend": "suspended", "deleted": "deleted"}[ev.Action]; !ok {
			out.Result, out.Reason = "ignored", fmt.Sprintf("installation %s changes no tenant", ev.Action)
			return nil
		}
	}

	cur, known, err := tx.Installation(ctx, in.ID)
	if err != nil {
		return err
	}
	out.Tenant = cur.Tenant
	switch {
	case known && cur.State == "deleted":
		out.Result, out.Reason = "stale", "the installation was deleted"
		if cur.Reason != "" {
			out.Reason += ": " + cur.Reason
		}
		return nil
	case cfg.Lookup == nil && known && state != "deleted" && in.UpdatedAt.Before(cur.UpdatedAt):
		// deleted is final whatever its updated_at says.
		out.Result, out.Reason = "stale", fmt.Sprintf("older than the event applied for it (%s)", cur.UpdatedAt.UTC().Format(time.RFC3339))
		return nil
	}
	next := jobstore.Installation{ID: in.ID, AccountID: in.Account.ID, AccountLogin: login, AccountType: kind,
		Tenant: cur.Tenant, State: cur.State, Reason: cur.Reason, UpdatedAt: in.UpdatedAt}
	if known && cur.UpdatedAt.After(next.UpdatedAt) {
		next.UpdatedAt = cur.UpdatedAt
	}

	// An account has one installation at a time, and GitHub numbers them
	// in order: an event of an older one is history.
	latest, hasLatest, err := tx.LatestInstallation(ctx, in.Account.ID, in.ID)
	if err != nil {
		return err
	}
	if hasLatest && latest.ID > in.ID {
		next.State, next.Reason = "deleted", fmt.Sprintf("superseded by installation %d", latest.ID)
		out.Result, out.Reason = "stale", "the account has a newer installation"
		return tx.PutInstallation(ctx, next)
	}
	if next.Tenant == "" && hasLatest {
		next.Tenant = latest.Tenant
	}

	switch state {
	case "active":
		reason, err := list(ctx, cfg, tx, &next, login, kind, latest, hasLatest)
		if err != nil {
			return err
		}
		if reason != "" {
			next.State, next.Reason = "refused", reason
			out.Result, out.Reason, out.Tenant = "refused", reason, ""
			return tx.PutInstallation(ctx, next)
		}
		if hasLatest && latest.State != "deleted" {
			latest.State, latest.Reason = "deleted", fmt.Sprintf("superseded by installation %d", in.ID)
			if err := tx.PutInstallation(ctx, latest); err != nil {
				return err
			}
		}
		next.State, next.Reason = "active", ""
		out.Result, out.Tenant = "listed", next.Tenant
	case "suspended", "deleted":
		if next.Tenant != "" {
			// Only if the entry is still this installation's: a newer one
			// on the account may hold the tenant now.
			if _, err := tx.DeleteTenant(ctx, next.Tenant, in.ID); err != nil {
				return err
			}
		}
		next.State, next.Reason = state, ""
		out.Result = map[string]string{"suspended": "suspended", "deleted": "removed"}[state]
	}
	return tx.PutInstallation(ctx, next)
}

// list lists the installation's tenant, or returns why it gets none.
func list(ctx context.Context, cfg Config, tx jobstore.OnboardTx, next *jobstore.Installation, login, kind string, latest jobstore.Installation, hasLatest bool) (string, error) {
	if kind != "Organization" {
		return "firerunner serves organizations; this is a personal account", nil
	}
	url := "https://github.com/" + login
	account := config.AccountOf(url)
	for _, r := range cfg.Reserved {
		if strings.EqualFold(r, account) {
			return "this organization is served by the install itself", nil
		}
	}
	entries, err := tx.Tenants(ctx)
	if err != nil {
		return "", err
	}
	if next.Tenant == "" {
		next.Tenant = tenantID(login, next.AccountID)
	}
	ours := func(installation int64) bool {
		return installation == next.ID || hasLatest && installation == latest.ID
	}
	tenants := make([]config.Tenant, 0, len(entries)+1)
	var mine *config.Tenant
	for id, raw := range entries {
		var t config.Tenant
		if err := json.Unmarshal(raw, &t); err != nil {
			return "", fmt.Errorf("tenant %q: %w", id, err)
		}
		switch {
		case id == next.Tenant && !ours(t.InstallationID):
			return fmt.Sprintf("tenant id %q is listed for another installation", id), nil
		case id == next.Tenant:
			mine = &t
			continue
		case config.AccountOf(t.URL) == account:
			return fmt.Sprintf("this organization is listed already, as tenant %q", id), nil
		}
		tenants = append(tenants, t)
	}
	t := config.Tenant{ID: next.Tenant, Tiers: cfg.Tiers, RunnerGroup: cfg.RunnerGroup}
	if mine != nil {
		// An entry an operator changed (limits, tiers) keeps the change.
		t = *mine
	}
	t.URL, t.InstallationID = url, next.ID
	if cfg.Check != nil {
		if err := cfg.Check(append(tenants, t)); err != nil {
			return fmt.Sprintf("the tenants list would not be served with it: %v", err), nil
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return "", tx.PutTenant(ctx, jobstore.Entry{ID: t.ID, Entry: b})
}

var plainLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// tenantID is the tenants list's id for an account: its login in lower
// case, when that is a valid id, else a<account id>.
func tenantID(login string, account int64) string {
	if l := strings.ToLower(login); plainLogin.MatchString(l) {
		return l
	}
	return "a" + strconv.FormatInt(account, 10)
}

func encode(o Outcome) string {
	b, _ := json.Marshal(o)
	return string(b)
}

// decodeDuplicate is a delivery applied before: what it did then.
func decodeDuplicate(out Outcome, recorded string) Outcome {
	var then Outcome
	if json.Unmarshal([]byte(recorded), &then) == nil {
		then.Delivery = out.Delivery
		out = then
	}
	out.Result, out.Reason = "duplicate", "delivered before: "+out.Result
	return out
}
