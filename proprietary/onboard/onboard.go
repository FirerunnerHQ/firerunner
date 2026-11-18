// Package onboard turns the hosted GitHub App's webhooks into the tenants
// list: an organization that installs the App becomes a tenant, served by
// the shards; one that suspends or uninstalls it stops being one. Each
// delivery is verified, applied once, and applied in order: one older than
// what was applied for its installation changes nothing (#170).
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

// Config is what a new tenant gets.
type Config struct {
	// Secret is the App's webhook secret, which signs every delivery.
	Secret []byte
	// Tiers are the catalog tiers every new tenant gets.
	Tiers []string
	// RunnerGroup is the runner group its scale sets go in; "" is the
	// daemon's --runner-group.
	RunnerGroup string
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
	// Result is created, unsuspended, suspended, removed, refused, stale (an
	// older event than one applied), ignored (an event onboarding does not
	// act on), or duplicate (a delivery applied before).
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
}

// Store is where deliveries are applied; a *jobstore.TenantStore.
type Store interface {
	Onboard(ctx context.Context, delivery, event string, installation int64, fn func(jobstore.OnboardTx) (string, error)) (string, error)
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

// installationPayload is the part of an installation event onboarding reads.
type installationPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"installation"`
}

// Handle verifies a delivery and applies it. A delivery whose signature
// does not hold is ErrSignature, and changes nothing; one that does not
// parse is an error. Events onboarding does not act on are ignored.
func Handle(ctx context.Context, cfg Config, store Store, d Delivery) (Outcome, error) {
	out := Outcome{Delivery: d.ID, Event: d.Event}
	if err := Verify(cfg.Secret, d.Body, d.Signature); err != nil {
		return out, err
	}
	if d.ID == "" {
		return out, errors.New("the delivery has no X-GitHub-Delivery id")
	}
	if d.Event != "installation" {
		out.Result = "ignored"
		out.Reason = "onboarding acts on installation events alone"
		return out, nil
	}
	var p installationPayload
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return out, fmt.Errorf("installation event: %w", err)
	}
	in := p.Installation
	if in.ID == 0 || in.Account.Login == "" || in.UpdatedAt.IsZero() {
		return out, errors.New("installation event: no installation id, account or updated_at")
	}
	out.Action, out.Installation, out.Account = p.Action, in.ID, in.Account.Login
	recorded, err := store.Onboard(ctx, d.ID, d.Event, in.ID, func(tx jobstore.OnboardTx) (string, error) {
		if err := apply(ctx, cfg, tx, p, &out); err != nil {
			return "", err
		}
		return encode(out), nil
	})
	if errors.Is(err, jobstore.ErrDelivered) {
		return decodeDuplicate(out, recorded), nil
	}
	return out, err
}

// apply moves the installation's state by one event, and its tenant with
// it. The caller holds the installation for the delivery's transaction.
func apply(ctx context.Context, cfg Config, tx jobstore.OnboardTx, p installationPayload, out *Outcome) error {
	in := p.Installation
	cur, known, err := tx.Installation(ctx, in.ID)
	if err != nil {
		return err
	}
	out.Tenant = cur.Tenant
	switch {
	case known && cur.State == "deleted":
		out.Result, out.Reason = "stale", "the installation was deleted"
		return nil
	case known && p.Action != "deleted" && in.UpdatedAt.Before(cur.UpdatedAt):
		// deleted is final whatever its updated_at says: GitHub need not
		// advance it for an uninstall, and nothing comes after one.
		out.Result, out.Reason = "stale", fmt.Sprintf("older than the event applied for it (%s)", cur.UpdatedAt.UTC().Format(time.RFC3339))
		return nil
	}
	next := jobstore.Installation{ID: in.ID, AccountLogin: in.Account.Login, AccountType: in.Account.Type,
		Tenant: cur.Tenant, State: cur.State, Reason: cur.Reason, UpdatedAt: in.UpdatedAt}

	switch p.Action {
	case "created", "unsuspend":
		if in.Account.Type != "Organization" {
			next.State, next.Reason = "refused", "firerunner serves organizations; this is a personal account"
			out.Result, out.Reason = "refused", next.Reason
			return tx.PutInstallation(ctx, next)
		}
		if next.Tenant == "" {
			next.Tenant = tenantID(in.Account.Login, in.ID)
		}
		other, taken, err := tx.TenantInstallation(ctx, next.Tenant)
		if err != nil {
			return err
		}
		if taken && other != in.ID {
			next.State, next.Reason = "refused", fmt.Sprintf("tenant id %q is another installation's", next.Tenant)
			out.Result, out.Reason, out.Tenant = "refused", next.Reason, ""
			next.Tenant = ""
			return tx.PutInstallation(ctx, next)
		}
		entry, err := tenantEntry(cfg, next.Tenant, in.Account.Login, in.ID)
		if err != nil {
			return err
		}
		if err := tx.PutTenant(ctx, entry); err != nil {
			return err
		}
		next.State, next.Reason = "active", ""
		out.Tenant = next.Tenant
		out.Result = map[string]string{"created": "created", "unsuspend": "unsuspended"}[p.Action]
	case "suspend", "deleted":
		if next.Tenant != "" {
			if err := tx.DeleteTenant(ctx, next.Tenant); err != nil {
				return err
			}
		}
		next.State = map[string]string{"suspend": "suspended", "deleted": "deleted"}[p.Action]
		next.Reason = ""
		out.Result = map[string]string{"suspend": "suspended", "deleted": "removed"}[p.Action]
	default:
		out.Result, out.Reason = "ignored", fmt.Sprintf("installation %s changes no tenant", p.Action)
		return nil
	}
	return tx.PutInstallation(ctx, next)
}

var plainLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// tenantID is the tenants list's id for an installation: its account's
// login in lower case, when that is a valid id, else i<installation id>.
func tenantID(login string, installation int64) string {
	if l := strings.ToLower(login); plainLogin.MatchString(l) {
		return l
	}
	return "i" + strconv.FormatInt(installation, 10)
}

// tenantEntry is a new tenant's entry: its organization, installation,
// the default tiers, and no limits, which are the conservative defaults.
func tenantEntry(cfg Config, id, login string, installation int64) (jobstore.Entry, error) {
	t := config.Tenant{ID: id, URL: "https://github.com/" + login, InstallationID: installation,
		Tiers: cfg.Tiers, RunnerGroup: cfg.RunnerGroup}
	b, err := json.Marshal(t)
	if err != nil {
		return jobstore.Entry{}, err
	}
	return jobstore.Entry{ID: id, Entry: b}, nil
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
