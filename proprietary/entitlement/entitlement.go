// Package entitlement keeps what each tenant may run, as billing last said,
// and decides from it whether the tenant's launches go ahead.
//
// A record per tenant names its account state (trial, active, past due,
// suspended), its budget state (ok, warning, capped) and how long the record
// holds (valid until). Billing writes the records; it knows prices, and this
// package never does. The daemon reads them every Interval and holds a
// tenant's launches when Decide says so: a held job stays queued at GitHub
// with its reason recorded, and starts once a record lets it.
//
// A record past its valid-until is judged by the account it last named,
// because the operator pays for the compute while billing cannot be reached:
// an active account goes on launching (fail open, as a stalled CI is worse
// than minutes billed late), unless its own spend cap was reached; any other,
// and a tenant with no record, is held (fail closed), so an outage of billing
// never hands out free compute to the accounts most likely to abuse it.
package entitlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

// Account is a tenant's standing with billing.
type Account string

// Accounts.
const (
	Trial     Account = "trial"
	Active    Account = "active"
	PastDue   Account = "past_due"
	Suspended Account = "suspended"
)

// Budget is where a tenant's spend stands against its cap.
type Budget string

// Budgets.
const (
	OK      Budget = "ok"
	Warning Budget = "warning"
	Capped  Budget = "capped"
)

// Record is one tenant's entitlement, as billing wrote it.
type Record struct {
	Plan    string  `json:"plan,omitempty"`
	Account Account `json:"account_state"`
	Budget  Budget  `json:"budget_state"`
	// ValidUntil is when the record stops holding as written (see Decide).
	ValidUntil time.Time `json:"valid_until"`
	// Reason is billing's words for a hold, shown with it ("the spend cap
	// for November is reached").
	Reason string `json:"reason,omitempty"`
	// Invalid says why a stored record could not be read; such a record
	// holds its tenant, as someone wrote it.
	Invalid string `json:"-"`
}

// Validate reports whether r names a known account and budget and a time it
// holds until.
func (r Record) Validate() error {
	switch r.Account {
	case Trial, Active, PastDue, Suspended:
	default:
		return fmt.Errorf("account_state %q: want trial, active, past_due or suspended", r.Account)
	}
	switch r.Budget {
	case OK, Warning, Capped:
	default:
		return fmt.Errorf("budget_state %q: want ok, warning or capped", r.Budget)
	}
	if r.ValidUntil.IsZero() {
		return errors.New("valid_until is required")
	}
	return nil
}

// Records are the entitlements, by tenant id.
type Records map[string]Record

// IDs returns the records' tenant ids, sorted.
func (rs Records) IDs() []string {
	ids := make([]string, 0, len(rs))
	for id := range rs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Decision is what a tenant's entitlement allows now.
type Decision struct {
	// Hold refuses every new launch of the tenant, for Why. It never stops
	// a running job: suspension does that (firerunner tenant suspend).
	Hold bool
	Why  string
	// FailOpen: the record has expired, and the tenant launches on the
	// active account it last named.
	FailOpen bool
	// Warning: the budget is near its cap.
	Warning bool
}

// Decide judges a tenant's record, ok false when it has none, at now.
//
//   - No record, or one that does not parse: held.
//   - A suspended account: held, expired or not; only a new record lifts it.
//   - A capped budget: held, expired or not; the cap is the customer's own.
//   - Expired, active: launches (FailOpen).
//   - Expired, trial or past due: held until billing confirms.
//   - Valid, trial, active or past due (billing's grace period): launches.
func Decide(r Record, ok bool, now time.Time) Decision {
	why := func(s string) string {
		if r.Reason != "" {
			return s + ": " + r.Reason
		}
		return s
	}
	switch {
	case !ok:
		return Decision{Hold: true, Why: "it has no entitlement record"}
	case r.Invalid != "":
		return Decision{Hold: true, Why: "its entitlement record does not parse: " + r.Invalid}
	case r.Account == Suspended:
		return Decision{Hold: true, Why: why("its account is suspended")}
	case r.Budget == Capped:
		return Decision{Hold: true, Why: why("its spend cap is reached")}
	case !now.Before(r.ValidUntil) && r.Account == Active:
		return Decision{FailOpen: true, Warning: r.Budget == Warning,
			Why: fmt.Sprintf("its entitlement expired at %s; launching on its last state, an active account", r.ValidUntil.UTC().Format(time.RFC3339))}
	case !now.Before(r.ValidUntil):
		return Decision{Hold: true, Why: fmt.Sprintf("its entitlement (account %s) expired at %s, and is held until billing renews it", r.Account, r.ValidUntil.UTC().Format(time.RFC3339))}
	case r.Account != Trial && r.Account != Active && r.Account != PastDue:
		return Decision{Hold: true, Why: fmt.Sprintf("its entitlement names an unknown account state %q", r.Account)}
	}
	return Decision{Warning: r.Budget == Warning}
}

// Store keeps the records.
type Store interface {
	fmt.Stringer
	Load(ctx context.Context) (Records, error)
	Put(ctx context.Context, id string, r Record) error
	Delete(ctx context.Context, id string) error
}

// CheckSpec reports whether spec names a store: "ssm:" and an absolute SSM
// path, or an absolute file path.
func CheckSpec(spec string) error { return tenantstate.CheckSpec(spec) }

// Open returns the store spec names. newSSM is asked for a client only for an
// SSM path.
func Open(spec string, newSSM func() (tenantstate.SSMAPI, error)) (Store, error) {
	if err := CheckSpec(spec); err != nil {
		return nil, err
	}
	if path, ok := strings.CutPrefix(spec, tenantstate.SSMPrefix); ok {
		api, err := newSSM()
		if err != nil {
			return nil, err
		}
		return &SSMStore{API: api, Path: path}, nil
	}
	return &FileStore{Path: spec}, nil
}

// decode reads one stored record; one that does not parse or validate comes
// back Invalid.
func decode(b []byte) Record {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{Invalid: err.Error()}
	}
	if err := r.Validate(); err != nil {
		return Record{Invalid: err.Error()}
	}
	return r
}

// FileStore keeps the records in one JSON file, {"<id>": {record}}. A
// missing file holds no record.
type FileStore struct{ Path string }

func (f *FileStore) String() string { return f.Path }

// Load reads the file.
func (f *FileStore) Load(context.Context) (Records, error) {
	raw, err := f.raw()
	if err != nil {
		return nil, err
	}
	rs := Records{}
	for id, m := range raw {
		rs[id] = decode(m)
	}
	return rs, nil
}

// raw reads the file's records as written, one JSON value per tenant.
func (f *FileStore) raw() (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return raw, nil
}

// Put writes the tenant's record, replacing the file whole.
func (f *FileStore) Put(_ context.Context, id string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return f.update(func(raw map[string]json.RawMessage) { raw[id] = b })
}

// Delete removes the tenant's record.
func (f *FileStore) Delete(_ context.Context, id string) error {
	return f.update(func(raw map[string]json.RawMessage) { delete(raw, id) })
}

// update changes one tenant's record and keeps every other as written, an
// unreadable one too, so that its writer can still see what it wrote.
func (f *FileStore) update(change func(map[string]json.RawMessage)) error {
	raw, err := f.raw()
	if err != nil {
		return err
	}
	change(raw)
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

// SSMStore keeps each record as a String parameter <Path>/<id> holding its
// JSON, as billing writes them: writing one tenant never touches another's.
type SSMStore struct {
	API  tenantstate.SSMAPI
	Path string
}

func (s *SSMStore) String() string { return tenantstate.SSMPrefix + s.Path }

// Load reads every parameter under the path.
func (s *SSMStore) Load(ctx context.Context) (Records, error) {
	rs := Records{}
	p := ssm.NewGetParametersByPathPaginator(s.API, &ssm.GetParametersByPathInput{Path: aws.String(s.Path)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, prm := range page.Parameters {
			rs[strings.TrimPrefix(aws.ToString(prm.Name), s.Path+"/")] = decode([]byte(aws.ToString(prm.Value)))
		}
	}
	return rs, nil
}

// Put writes the tenant's parameter.
func (s *SSMStore) Put(ctx context.Context, id string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.API.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String(s.Path + "/" + id), Value: aws.String(string(b)),
		Type: types.ParameterTypeString, Overwrite: aws.Bool(true),
		Description: aws.String("firerunner: what this tenant may run, as billing last said"),
	})
	return err
}

// Delete removes the tenant's parameter; one already gone is no error.
func (s *SSMStore) Delete(ctx context.Context, id string) error {
	_, err := s.API.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(s.Path + "/" + id)})
	var nf *types.ParameterNotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}
