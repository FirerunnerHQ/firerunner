// Package onboard turns the hosted GitHub App's webhooks into the tenants
// list: an organization that installs the App becomes a tenant, served by
// the shards; one that suspends or uninstalls it stops being one (#170).
//
// A webhook is a trigger, not the truth: GitHub delivers each at least
// once, in any order, and does not retry a failed one by itself. So a
// delivery is verified and applied once, beside no other delivery, to its
// account: an organization has one installation of the App at a time, and
// keeps its tenant id across reinstalls. With Config.Lookup it applies the
// installation's state as GitHub reports it then, not as the event says;
// without, an event older than the one applied for its installation
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
	// KeepGroup is a delivery after which the installation's runner group
	// is to be kept (Keeper.Keep): it is listed, or its repositories
	// changed.
	KeepGroup bool `json:"keep_group,omitempty"`
}

// Store is where deliveries are applied; a *jobstore.TenantStore.
type Store interface {
	Onboard(ctx context.Context, delivery, event string, fn func(jobstore.OnboardTx) (string, error)) (string, error)
}

// ErrSignature is a delivery whose signature does not hold: not GitHub's,
// or for another secret.
var ErrSignature = errors.New("the webhook signature does not hold")

// ErrMalformed is a delivery that will never apply, however often it is
// tried: not what GitHub sends.
var ErrMalformed = errors.New("the delivery is malformed")

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
// does not hold is ErrSignature, and one that does not parse ErrMalformed:
// neither changes anything, nor ever will. One that fails to apply is
// another error, and changes nothing either, so it can be retried. Events onboarding does not act on are ignored.
func Handle(ctx context.Context, cfg Config, store Store, d Delivery) (Outcome, error) {
	out := Outcome{Delivery: d.ID, Event: d.Event}
	if err := Verify(cfg.Secret, d.Body, d.Signature); err != nil {
		return out, err
	}
	if d.ID == "" {
		return out, fmt.Errorf("%w: no X-GitHub-Delivery id", ErrMalformed)
	}
	if d.Event == "installation_repositories" {
		// Which repositories it may access is GitHub's to say when the
		// group is kept; the event says only that it changed.
		var ev struct {
			Installation struct {
				ID int64 `json:"id"`
			} `json:"installation"`
		}
		if err := json.Unmarshal(d.Body, &ev); err != nil || ev.Installation.ID <= 0 {
			return out, fmt.Errorf("%w: installation_repositories event with no installation id (%v)", ErrMalformed, err)
		}
		out.Installation, out.KeepGroup = ev.Installation.ID, true
		out.Result, out.Reason = "ignored", "changes the runner group's repositories, not the tenant"
		return out, nil
	}
	if d.Event != "installation" {
		out.Result, out.Reason = "ignored", "onboarding acts on installation events alone"
		return out, nil
	}
	var ev installationEvent
	if err := json.Unmarshal(d.Body, &ev); err != nil {
		return out, fmt.Errorf("%w: installation event: %v", ErrMalformed, err)
	}
	in := ev.Installation
	if in.ID <= 0 || in.Account.ID <= 0 || in.Account.Login == "" || in.UpdatedAt.IsZero() {
		return out, fmt.Errorf("%w: installation event with no installation id, account or updated_at", ErrMalformed)
	}
	out.Action, out.Installation, out.Account = ev.Action, in.ID, in.Account.Login
	recorded, err := store.Onboard(ctx, d.ID, d.Event, func(tx jobstore.OnboardTx) (string, error) {
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
// tenant with it. No other delivery runs beside it.
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
		case !o.Found && ev.Action != "deleted":
			// deleted is final: one lookup racing a creation, or a
			// transient 404, must not make it so. A retry decides.
			return fmt.Errorf("installation %d: GitHub does not have it, but the event is %s", in.ID, ev.Action)
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
	// in order: an older one is gone once a newer one exists, whatever its
	// own events say, or whether they arrive at all.
	latest, hasLatest, err := tx.LatestInstallation(ctx, in.Account.ID, in.ID)
	if err != nil {
		return err
	}
	if next.Tenant == "" && hasLatest {
		next.Tenant = latest.Tenant
	}
	if hasLatest && latest.ID > in.ID {
		if err := supersede(ctx, tx, &next, latest.ID); err != nil {
			return err
		}
		out.Result, out.Reason = "stale", "the account has a newer installation"
		return tx.PutInstallation(ctx, next)
	}
	older := hasLatest && latest.State != "deleted"

	switch state {
	case "active":
		reason, listed, err := list(ctx, cfg, tx, &next, login, kind, latest, hasLatest)
		if err != nil {
			return err
		}
		switch {
		case reason != "" && listed:
			// An entry listed before stays as it was, and so does the
			// installation's state, which keeps it from an import.
			next.State, next.Reason = "active", reason
			out.Result, out.Reason, out.Tenant = "refused", reason+"; its tenant stays listed as it was", next.Tenant
		case reason != "":
			next.State, next.Reason = "refused", reason
			out.Result, out.Reason, out.Tenant = "refused", reason, ""
		default:
			next.State, next.Reason = "active", ""
			out.Result, out.Tenant = "listed", next.Tenant
		}
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
	if older {
		// This installation exists, so the older one does not: its tenant
		// goes unless this one just took it over.
		if err := supersede(ctx, tx, &latest, in.ID); err != nil {
			return err
		}
		if err := tx.PutInstallation(ctx, latest); err != nil {
			return err
		}
	}
	out.KeepGroup = next.State == "active"
	return tx.PutInstallation(ctx, next)
}

// supersede marks an installation deleted for a newer one on its account,
// and deletes its tenant if its entry is still the installation's.
func supersede(ctx context.Context, tx jobstore.OnboardTx, in *jobstore.Installation, newer int64) error {
	if in.Tenant != "" {
		if _, err := tx.DeleteTenant(ctx, in.Tenant, in.ID); err != nil {
			return err
		}
	}
	in.State, in.Reason = "deleted", fmt.Sprintf("superseded by installation %d", newer)
	return nil
}

// list lists the installation's tenant, or returns why it gets none, and
// whether its tenant was listed before, which it then leaves as it was.
func list(ctx context.Context, cfg Config, tx jobstore.OnboardTx, next *jobstore.Installation, login, kind string, latest jobstore.Installation, hasLatest bool) (string, bool, error) {
	entries, err := tx.Tenants(ctx)
	if err != nil {
		return "", false, err
	}
	ours := func(installation int64) bool {
		return installation == next.ID || hasLatest && installation == latest.ID
	}
	parsed := make(map[string]config.Tenant, len(entries))
	for id, raw := range entries {
		var t config.Tenant
		if err := json.Unmarshal(raw, &t); err != nil {
			return "", false, fmt.Errorf("tenant %q: %w", id, err)
		}
		parsed[id] = t
	}
	mine, listed := parsed[next.Tenant]
	listed = listed && next.Tenant != "" && ours(mine.InstallationID)

	if kind != "Organization" {
		return "firerunner serves organizations; this is a personal account", listed, nil
	}
	url := "https://github.com/" + login
	account := config.AccountOf(url)
	for _, r := range cfg.Reserved {
		if strings.EqualFold(r, account) {
			return "this organization is served by the install itself", listed, nil
		}
	}
	// taken says why id cannot be this account's: listed for another
	// installation, or given to another organization, which may list it
	// again.
	taken := func(id string) (string, error) {
		if t, ok := parsed[id]; ok && !ours(t.InstallationID) {
			return "listed for another installation", nil
		}
		other, err := tx.TenantTaken(ctx, id, next.AccountID)
		if err != nil || !other {
			return "", err
		}
		return "another organization's", nil
	}
	if next.Tenant == "" {
		candidates := []string{tenantID(login, next.AccountID), "a" + strconv.FormatInt(next.AccountID, 10)}
		for _, id := range candidates {
			why, err := taken(id)
			if err != nil {
				return "", false, err
			}
			if why == "" {
				next.Tenant = id
				break
			}
		}
		if next.Tenant == "" {
			return fmt.Sprintf("tenant ids %q and %q are both taken", candidates[0], candidates[1]), false, nil
		}
	} else if why, err := taken(next.Tenant); err != nil || why != "" {
		return fmt.Sprintf("tenant id %q is %s", next.Tenant, why), listed, err
	}

	tenants := make([]config.Tenant, 0, len(parsed)+1)
	for id, t := range parsed {
		if id == next.Tenant {
			continue
		}
		if config.AccountOf(t.URL) == account {
			return fmt.Sprintf("this organization is listed already, as tenant %q", id), listed, nil
		}
		tenants = append(tenants, t)
	}
	// Not served until the keeper has made its group ready and set its id
	// (jobstore.RecordGroup): by name alone, its scale sets could go in a
	// group the organization made for itself.
	notReady := int64(0)
	t := config.Tenant{ID: next.Tenant, Tiers: cfg.Tiers, RunnerGroup: cfg.RunnerGroup, RunnerGroupID: &notReady}
	if listed {
		// An entry an operator changed (limits, tiers) keeps the change.
		t = mine
	}
	t.URL, t.InstallationID = url, next.ID
	if cfg.Check != nil {
		if err := cfg.Check(append(tenants, t)); err != nil {
			return fmt.Sprintf("the tenants list would not be served with it: %v", err), listed, nil
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", false, err
	}
	return "", false, tx.PutTenant(ctx, jobstore.Entry{ID: t.ID, Entry: b})
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
