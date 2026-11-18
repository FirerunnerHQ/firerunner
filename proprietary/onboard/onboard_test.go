package onboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

var secret = []byte("webhook-secret")

// fakeStore applies deliveries as the job store does: once each, all or
// nothing.
type fakeStore struct {
	installations map[int64]jobstore.Installation
	tenants       map[string]jobstore.Entry
	delivered     map[string]string
	failPut       error
}

func newFake() *fakeStore {
	return &fakeStore{installations: map[int64]jobstore.Installation{}, tenants: map[string]jobstore.Entry{}, delivered: map[string]string{}}
}

func (f *fakeStore) Onboard(_ context.Context, delivery, _ string, _ int64, fn func(jobstore.OnboardTx) (string, error)) (string, error) {
	if out, ok := f.delivered[delivery]; ok {
		return out, jobstore.ErrDelivered
	}
	tx := &fakeTx{f: f, installations: maps.Clone(f.installations), tenants: maps.Clone(f.tenants)}
	out, err := fn(tx)
	if err != nil {
		return "", err
	}
	f.installations, f.tenants, f.delivered[delivery] = tx.installations, tx.tenants, out
	return out, nil
}

type fakeTx struct {
	f             *fakeStore
	installations map[int64]jobstore.Installation
	tenants       map[string]jobstore.Entry
}

func (t *fakeTx) Installation(_ context.Context, id int64) (jobstore.Installation, bool, error) {
	in, ok := t.installations[id]
	return in, ok, nil
}

func (t *fakeTx) PutInstallation(_ context.Context, in jobstore.Installation) error {
	t.installations[in.ID] = in
	return nil
}

func (t *fakeTx) TenantInstallation(_ context.Context, tenant string) (int64, bool, error) {
	e, ok := t.tenants[tenant]
	if !ok {
		return 0, false, nil
	}
	var c config.Tenant
	if err := json.Unmarshal(e.Entry, &c); err != nil {
		return 0, false, err
	}
	return c.InstallationID, true, nil
}

func (t *fakeTx) PutTenant(_ context.Context, e jobstore.Entry) error {
	if t.f.failPut != nil {
		return t.f.failPut
	}
	t.tenants[e.ID] = e
	return nil
}

func (t *fakeTx) DeleteTenant(_ context.Context, tenant string) error {
	delete(t.tenants, tenant)
	return nil
}

var cfg = Config{Secret: secret, Tiers: []string{"linux-2"}, RunnerGroup: "firerunner"}

var base = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

var serial int

// event is a signed installation delivery: action on installation id of
// login (an Organization unless kind says otherwise), updated at base+at.
func event(action string, id int64, login, kind string, at time.Duration) Delivery {
	if kind == "" {
		kind = "Organization"
	}
	body := fmt.Sprintf(`{"action":%q,"installation":{"id":%d,"account":{"login":%q,"type":%q},"updated_at":%q}}`,
		action, id, login, kind, base.Add(at).Format(time.RFC3339))
	serial++
	return sign(Delivery{ID: fmt.Sprintf("d-%d", serial), Event: "installation", Body: []byte(body)})
}

func sign(d Delivery) Delivery {
	mac := hmac.New(sha256.New, secret)
	mac.Write(d.Body)
	d.Signature = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return d
}

func handle(t *testing.T, s *fakeStore, d Delivery) Outcome {
	t.Helper()
	out, err := Handle(context.Background(), cfg, s, d)
	if err != nil {
		t.Fatalf("Handle(%s): %v", d.Body, err)
	}
	return out
}

func tenant(t *testing.T, s *fakeStore, id string) (config.Tenant, bool) {
	t.Helper()
	e, ok := s.tenants[id]
	if !ok {
		return config.Tenant{}, false
	}
	var c config.Tenant
	if err := json.Unmarshal(e.Entry, &c); err != nil {
		t.Fatal(err)
	}
	return c, true
}

// An organization that installs the App becomes a tenant with the default
// tiers, under its login, and one that uninstalls it stops being one.
func TestInstallThenUninstall(t *testing.T) {
	s := newFake()
	out := handle(t, s, event("created", 41, "Momiji-Labs", "", 0))
	if out.Result != "created" || out.Tenant != "momiji-labs" {
		t.Fatalf("outcome %+v", out)
	}
	c, ok := tenant(t, s, "momiji-labs")
	if !ok || c.URL != "https://github.com/Momiji-Labs" || c.InstallationID != 41 || len(c.Tiers) != 1 || c.Tiers[0] != "linux-2" || c.RunnerGroup != "firerunner" || c.Limits != nil {
		t.Fatalf("tenant %+v", c)
	}
	if _, err := config.ParseTenants([]byte("[" + string(s.tenants["momiji-labs"].Entry) + "]")); err != nil {
		t.Fatalf("the entry onboarding writes does not parse as a tenants list: %v", err)
	}
	out = handle(t, s, event("deleted", 41, "Momiji-Labs", "", time.Minute))
	if out.Result != "removed" || out.Tenant != "momiji-labs" {
		t.Fatalf("outcome %+v", out)
	}
	if _, ok := tenant(t, s, "momiji-labs"); ok {
		t.Fatal("the tenant outlived its installation")
	}
	if s.installations[41].State != "deleted" {
		t.Fatalf("installation %+v", s.installations[41])
	}
}

// A personal account gets no tenant, and the refusal says why.
func TestPersonalAccountRefused(t *testing.T) {
	s := newFake()
	out := handle(t, s, event("created", 42, "someone", "User", 0))
	if out.Result != "refused" || out.Reason == "" || out.Tenant != "" {
		t.Fatalf("outcome %+v", out)
	}
	if len(s.tenants) != 0 {
		t.Fatalf("tenants %v", s.tenants)
	}
	if in := s.installations[42]; in.State != "refused" || in.Reason == "" {
		t.Fatalf("installation %+v", in)
	}
	if out := handle(t, s, event("unsuspend", 42, "someone", "User", time.Minute)); out.Result != "refused" || len(s.tenants) != 0 {
		t.Fatalf("unsuspend of a personal account: %+v", out)
	}
}

// A delivery GitHub sends again changes nothing, and reports what it did
// the first time.
func TestReplayChangesNothing(t *testing.T) {
	s := newFake()
	created := event("created", 43, "acme", "", 0)
	handle(t, s, created)
	handle(t, s, event("suspend", 43, "acme", "", time.Minute))
	out := handle(t, s, created)
	if out.Result != "duplicate" || out.Reason != "delivered before: created" || out.Tenant != "acme" {
		t.Fatalf("outcome %+v", out)
	}
	if _, ok := tenant(t, s, "acme"); ok {
		t.Fatal("a replayed creation revived a suspended tenant")
	}
}

// An event older than one applied for its installation changes nothing,
// whatever order GitHub delivers them in.
func TestOutOfOrder(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 44, "acme", "", 0))
	handle(t, s, event("unsuspend", 44, "acme", "", 2*time.Minute))
	out := handle(t, s, event("suspend", 44, "acme", "", time.Minute))
	if out.Result != "stale" {
		t.Fatalf("outcome %+v", out)
	}
	if _, ok := tenant(t, s, "acme"); !ok {
		t.Fatal("a stale suspension removed the tenant")
	}
	if !s.installations[44].UpdatedAt.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("installation %+v", s.installations[44])
	}
}

// An uninstall is final: it applies whatever its updated_at, and nothing
// delivered after it brings the tenant back.
func TestDeletedIsFinal(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 45, "acme", "", 0))
	handle(t, s, event("suspend", 45, "acme", "", 2*time.Minute))
	if out := handle(t, s, event("deleted", 45, "acme", "", time.Minute)); out.Result != "removed" {
		t.Fatalf("an uninstall older than the suspension: %+v", out)
	}
	for _, action := range []string{"created", "unsuspend"} {
		if out := handle(t, s, event(action, 45, "acme", "", time.Hour)); out.Result != "stale" {
			t.Fatalf("%s after the uninstall: %+v", action, out)
		}
	}
	if len(s.tenants) != 0 || s.installations[45].State != "deleted" {
		t.Fatalf("tenants %v, installation %+v", s.tenants, s.installations[45])
	}
}

// Suspending removes the tenant; unsuspending brings it back under the same
// id, with its entry as onboarding writes it.
func TestSuspendUnsuspend(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 46, "acme", "", 0))
	if out := handle(t, s, event("suspend", 46, "acme", "", time.Minute)); out.Result != "suspended" || len(s.tenants) != 0 {
		t.Fatalf("suspend: %+v, tenants %v", out, s.tenants)
	}
	if out := handle(t, s, event("unsuspend", 46, "acme", "", 2*time.Minute)); out.Result != "unsuspended" || out.Tenant != "acme" {
		t.Fatalf("unsuspend: %+v", out)
	}
	if c, ok := tenant(t, s, "acme"); !ok || c.InstallationID != 46 {
		t.Fatalf("tenant %+v", c)
	}
}

// Two installations never share a tenant id: the second is refused rather
// than taking the first one's tenant over.
func TestTenantIDTaken(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 47, "acme", "", 0))
	out := handle(t, s, event("created", 48, "ACME", "", 0))
	if out.Result != "refused" || out.Tenant != "" {
		t.Fatalf("outcome %+v", out)
	}
	if c, _ := tenant(t, s, "acme"); c.InstallationID != 47 {
		t.Fatalf("the first installation's tenant was taken over: %+v", c)
	}
}

// A login that is not a valid tenant id gets one from its installation.
func TestTenantIDFallback(t *testing.T) {
	for login, want := range map[string]string{
		"acme":                                 "acme",
		"Acme-Corp":                            "acme-corp",
		"a-login-that-is-far-longer-than-32ch": "i49",
		"-dash":                                "i49",
	} {
		if got := tenantID(login, 49); got != want {
			t.Errorf("tenantID(%q) = %q, want %q", login, got, want)
		}
		if !tenantstate.ValidID(tenantID(login, 49)) {
			t.Errorf("tenantID(%q) is not a valid tenant id", login)
		}
	}
}

// A delivery whose signature does not hold changes nothing.
func TestSignature(t *testing.T) {
	s := newFake()
	d := event("created", 50, "acme", "", 0)
	for name, bad := range map[string]Delivery{
		"no signature":   {ID: d.ID, Event: d.Event, Body: d.Body},
		"another secret": {ID: d.ID, Event: d.Event, Body: d.Body, Signature: "sha256=" + hex.EncodeToString(make([]byte, 32))},
		"not hex":        {ID: d.ID, Event: d.Event, Body: d.Body, Signature: "sha256=zz"},
		"sha1":           {ID: d.ID, Event: d.Event, Body: d.Body, Signature: "sha1=" + d.Signature[7:]},
		"body changed":   {ID: d.ID, Event: d.Event, Body: append([]byte(" "), d.Body...), Signature: d.Signature},
	} {
		if _, err := Handle(context.Background(), cfg, s, bad); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: err %v, want ErrSignature", name, err)
		}
	}
	if _, err := Handle(context.Background(), Config{Tiers: cfg.Tiers}, s, d); !errors.Is(err, ErrSignature) {
		t.Errorf("no secret configured: err %v, want ErrSignature", err)
	}
	if len(s.tenants) != 0 || len(s.delivered) != 0 {
		t.Fatalf("a bad signature changed something: %v %v", s.tenants, s.delivered)
	}
}

// Other events are ignored, and so are installation actions that change no
// tenant.
func TestIgnored(t *testing.T) {
	s := newFake()
	if out := handle(t, s, sign(Delivery{ID: "p", Event: "ping", Body: []byte(`{"zen":"x"}`)})); out.Result != "ignored" {
		t.Fatalf("ping: %+v", out)
	}
	if out := handle(t, s, event("new_permissions_accepted", 51, "acme", "", 0)); out.Result != "ignored" || len(s.installations) != 0 {
		t.Fatalf("new_permissions_accepted: %+v", out)
	}
}

// A failed write applies nothing, so GitHub's or the queue's retry of the
// same delivery is applied in full.
func TestFailureAppliesNothing(t *testing.T) {
	s := newFake()
	d := event("created", 52, "acme", "", 0)
	s.failPut = errors.New("database down")
	if _, err := Handle(context.Background(), cfg, s, d); err == nil {
		t.Fatal("no error")
	}
	if len(s.installations) != 0 || len(s.delivered) != 0 {
		t.Fatalf("a failed delivery was partly applied: %v %v", s.installations, s.delivered)
	}
	s.failPut = nil
	if out := handle(t, s, d); out.Result != "created" {
		t.Fatalf("retry: %+v", out)
	}
}

// A payload missing what ordering needs is an error, not a guess.
func TestMalformed(t *testing.T) {
	s := newFake()
	for _, body := range []string{`{`, `{"action":"created","installation":{"id":53,"account":{"login":"acme","type":"Organization"}}}`} {
		if _, err := Handle(context.Background(), cfg, s, sign(Delivery{ID: "m", Event: "installation", Body: []byte(body)})); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
}
