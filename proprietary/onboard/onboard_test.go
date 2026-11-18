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
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/schedule"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

var secret = []byte("webhook-secret")

// fakeStore applies deliveries as the job store does: once each, all or
// nothing.
type fakeStore struct {
	installations map[int64]jobstore.Installation
	tenants       map[string]json.RawMessage
	delivered     map[string]string
	failPut       error
}

func newFake() *fakeStore {
	return &fakeStore{installations: map[int64]jobstore.Installation{}, tenants: map[string]json.RawMessage{}, delivered: map[string]string{}}
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
	tenants       map[string]json.RawMessage
}

func (t *fakeTx) Installation(_ context.Context, id int64) (jobstore.Installation, bool, error) {
	in, ok := t.installations[id]
	return in, ok, nil
}

func (t *fakeTx) LatestInstallation(_ context.Context, account, except int64) (jobstore.Installation, bool, error) {
	var best jobstore.Installation
	for id, in := range t.installations {
		if in.AccountID == account && id != except && id > best.ID {
			best = in
		}
	}
	return best, best.ID != 0, nil
}

func (t *fakeTx) PutInstallation(_ context.Context, in jobstore.Installation) error {
	t.installations[in.ID] = in
	return nil
}

func (t *fakeTx) Tenants(context.Context) (map[string]json.RawMessage, error) {
	return maps.Clone(t.tenants), nil
}

func (t *fakeTx) PutTenant(_ context.Context, e jobstore.Entry) error {
	if t.f.failPut != nil {
		return t.f.failPut
	}
	t.tenants[e.ID] = e.Entry
	return nil
}

func (t *fakeTx) DeleteTenant(_ context.Context, tenant string, installation int64) (bool, error) {
	var c config.Tenant
	if e, ok := t.tenants[tenant]; !ok || json.Unmarshal(e, &c) != nil || c.InstallationID != installation {
		return false, nil
	}
	delete(t.tenants, tenant)
	return true, nil
}

var cfg = Config{Secret: secret, Tiers: []string{"linux-2"}, RunnerGroup: "firerunner"}

var base = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

var serial int

// event is a signed installation delivery: action on installation id of
// account 900 named login (an Organization unless kind says otherwise),
// updated at base+at.
func event(action string, id int64, login, kind string, at time.Duration) Delivery {
	return accountEvent(action, id, 900, login, kind, at)
}

func accountEvent(action string, id, account int64, login, kind string, at time.Duration) Delivery {
	if kind == "" {
		kind = "Organization"
	}
	body := fmt.Sprintf(`{"action":%q,"installation":{"id":%d,"account":{"id":%d,"login":%q,"type":%q},"updated_at":%q}}`,
		action, id, account, login, kind, base.Add(at).Format(time.RFC3339))
	serial++
	return sign(Delivery{ID: fmt.Sprintf("d-%d", serial), Event: "installation", Body: []byte(body)})
}

func sign(d Delivery) Delivery {
	mac := hmac.New(sha256.New, secret)
	mac.Write(d.Body)
	d.Signature = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return d
}

func handleWith(t *testing.T, c Config, s *fakeStore, d Delivery) Outcome {
	t.Helper()
	out, err := Handle(context.Background(), c, s, d)
	if err != nil {
		t.Fatalf("Handle(%s): %v", d.Body, err)
	}
	return out
}

func handle(t *testing.T, s *fakeStore, d Delivery) Outcome {
	t.Helper()
	return handleWith(t, cfg, s, d)
}

func tenant(t *testing.T, s *fakeStore, id string) (config.Tenant, bool) {
	t.Helper()
	e, ok := s.tenants[id]
	if !ok {
		return config.Tenant{}, false
	}
	var c config.Tenant
	if err := json.Unmarshal(e, &c); err != nil {
		t.Fatal(err)
	}
	return c, true
}

// An organization that installs the App becomes a tenant with the default
// tiers, under its login, and one that uninstalls it stops being one.
func TestInstallThenUninstall(t *testing.T) {
	s := newFake()
	out := handle(t, s, event("created", 41, "Momiji-Labs", "", 0))
	if out.Result != "listed" || out.Tenant != "momiji-labs" {
		t.Fatalf("outcome %+v", out)
	}
	c, ok := tenant(t, s, "momiji-labs")
	if !ok || c.URL != "https://github.com/Momiji-Labs" || c.InstallationID != 41 || len(c.Tiers) != 1 || c.Tiers[0] != "linux-2" || c.RunnerGroup != "firerunner" || c.Limits != nil {
		t.Fatalf("tenant %+v", c)
	}
	if _, err := config.ParseTenants([]byte("[" + string(s.tenants["momiji-labs"]) + "]")); err != nil {
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
	if out.Result != "refused" || !strings.Contains(out.Reason, "personal account") || out.Tenant != "" {
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
	if out.Result != "duplicate" || out.Reason != "delivered before: listed" || out.Tenant != "acme" {
		t.Fatalf("outcome %+v", out)
	}
	if _, ok := tenant(t, s, "acme"); ok {
		t.Fatal("a replayed creation revived a suspended tenant")
	}
}

// Without Lookup, an event older than one applied for its installation
// changes nothing, whatever order GitHub delivers them in.
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
// id.
func TestSuspendUnsuspend(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 46, "acme", "", 0))
	if out := handle(t, s, event("suspend", 46, "acme", "", time.Minute)); out.Result != "suspended" || len(s.tenants) != 0 {
		t.Fatalf("suspend: %+v, tenants %v", out, s.tenants)
	}
	if out := handle(t, s, event("unsuspend", 46, "acme", "", 2*time.Minute)); out.Result != "listed" || out.Tenant != "acme" {
		t.Fatalf("unsuspend: %+v", out)
	}
	if c, ok := tenant(t, s, "acme"); !ok || c.InstallationID != 46 {
		t.Fatalf("tenant %+v", c)
	}
}

// An organization that uninstalls and installs again keeps its tenant, and
// the old installation's late events never touch the new one's, in either
// order.
func TestReinstall(t *testing.T) {
	for _, newFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("new installation first %v", newFirst), func(t *testing.T) {
			s := newFake()
			handle(t, s, event("created", 60, "acme", "", 0))
			handle(t, s, event("suspend", 60, "acme", "", time.Minute))
			oldDeleted := event("deleted", 60, "acme", "", 2*time.Minute)
			if !newFirst {
				handle(t, s, oldDeleted)
			}
			if out := handle(t, s, event("created", 61, "acme", "", 3*time.Minute)); out.Result != "listed" || out.Tenant != "acme" {
				t.Fatalf("reinstall: %+v", out)
			}
			if newFirst {
				if out := handle(t, s, oldDeleted); out.Result != "stale" {
					t.Fatalf("the old installation's uninstall after the reinstall: %+v", out)
				}
			}
			for _, action := range []string{"suspend", "deleted", "created"} {
				if out := handle(t, s, event(action, 60, "acme", "", time.Hour)); out.Result != "stale" {
					t.Fatalf("old installation's %s: %+v", action, out)
				}
			}
			if c, ok := tenant(t, s, "acme"); !ok || c.InstallationID != 61 {
				t.Fatalf("the reinstalled organization's tenant: %+v, %v", c, ok)
			}
			if old := s.installations[60]; old.State != "deleted" {
				t.Fatalf("old installation %+v", old)
			}
		})
	}
}

// A newer installation takes over an older one still active (its
// uninstall not delivered yet), and the older one is then history.
func TestReinstallSupersedes(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 62, "acme", "", 0))
	handle(t, s, event("created", 63, "acme", "", time.Minute))
	if c, _ := tenant(t, s, "acme"); c.InstallationID != 63 {
		t.Fatalf("tenant %+v", c)
	}
	if old := s.installations[62]; old.State != "deleted" || !strings.Contains(old.Reason, "superseded") {
		t.Fatalf("old installation %+v", old)
	}
	if out := handle(t, s, event("deleted", 62, "acme", "", 2*time.Minute)); out.Result != "stale" {
		t.Fatalf("old installation's uninstall: %+v", out)
	}
	if _, ok := tenant(t, s, "acme"); !ok {
		t.Fatal("the old installation's uninstall removed the new one's tenant")
	}
}

// An older installation never seen before, its creation delivered after a
// newer one's, is history: it takes nothing over.
func TestLateOlderInstallation(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 71, "acme", "", time.Minute))
	if out := handle(t, s, event("created", 70, "acme", "", 0)); out.Result != "stale" {
		t.Fatalf("outcome %+v", out)
	}
	if c, _ := tenant(t, s, "acme"); c.InstallationID != 71 {
		t.Fatalf("tenant %+v", c)
	}
	if old := s.installations[70]; old.State != "deleted" {
		t.Fatalf("old installation %+v", old)
	}
}

// An organization renamed between an uninstall and a reinstall keeps its
// tenant id, whose scale sets and leftovers are named by it.
func TestReinstallAfterRename(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 72, "acme", "", 0))
	handle(t, s, event("deleted", 72, "acme", "", time.Minute))
	out := handle(t, s, event("created", 73, "acme-renamed", "", 2*time.Minute))
	if out.Result != "listed" || out.Tenant != "acme" {
		t.Fatalf("outcome %+v", out)
	}
	if c, _ := tenant(t, s, "acme"); c.URL != "https://github.com/acme-renamed" || c.InstallationID != 73 {
		t.Fatalf("tenant %+v", c)
	}
}

// A tenant id or an organization an operator listed is never taken over;
// nor is the install's own organization.
func TestConflictsRefused(t *testing.T) {
	operator := func(id, url string, installation int64) json.RawMessage {
		b, _ := json.Marshal(config.Tenant{ID: id, URL: url, InstallationID: installation, Tiers: []string{"linux-2"}})
		return b
	}
	cases := map[string]struct {
		tenants map[string]json.RawMessage
		login   string
		want    string
	}{
		"id of another organization":  {map[string]json.RawMessage{"acme": operator("acme", "https://github.com/acme-old", 5)}, "acme", "listed for another installation"},
		"organization listed already": {map[string]json.RawMessage{"big": operator("big", "https://github.com/ACME/repo", 5)}, "acme", "listed already, as tenant \"big\""},
		"the install's own":           {nil, "Home", "served by the install itself"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newFake()
			maps.Copy(s.tenants, c.tenants)
			before := maps.Clone(s.tenants)
			conf := cfg
			conf.Reserved = []string{"github.com/home"}
			out := handleWith(t, conf, s, event("created", 64, c.login, "", 0))
			if out.Result != "refused" || !strings.Contains(out.Reason, c.want) {
				t.Fatalf("outcome %+v, want refused: %s", out, c.want)
			}
			if !maps.EqualFunc(s.tenants, before, func(a, b json.RawMessage) bool { return string(a) == string(b) }) {
				t.Fatalf("tenants changed: %v", s.tenants)
			}
		})
	}
}

// A refused installation is listed by its next event once the conflict is
// gone: a refusal is not final.
func TestRefusalRetried(t *testing.T) {
	s := newFake()
	s.tenants["big"] = json.RawMessage(`{"id":"big","url":"https://github.com/acme","installation_id":5,"tiers":["linux-2"]}`)
	if out := handle(t, s, event("created", 65, "acme", "", 0)); out.Result != "refused" {
		t.Fatalf("outcome %+v", out)
	}
	delete(s.tenants, "big")
	if out := handle(t, s, event("new_permissions_accepted", 65, "acme", "", time.Minute)); out.Result != "ignored" {
		t.Fatalf("without Lookup only a state-changing event applies: %+v", out)
	}
	if out := handle(t, s, event("unsuspend", 65, "acme", "", 2*time.Minute)); out.Result != "listed" {
		t.Fatalf("outcome %+v", out)
	}
}

// A list the daemon would refuse is never written: the installation is
// refused, and every other tenant keeps being served.
func TestCheckedList(t *testing.T) {
	s := newFake()
	s.tenants["other"] = json.RawMessage(`{"id":"other","url":"https://github.com/other","installation_id":5,"tiers":["linux-2"]}`)
	conf := cfg
	var seen []config.Tenant
	conf.Check = func(ts []config.Tenant) error {
		seen = ts
		return errors.New("tier linux-2 is not in the catalog")
	}
	out := handleWith(t, conf, s, event("created", 66, "acme", "", 0))
	if out.Result != "refused" || !strings.Contains(out.Reason, "not in the catalog") {
		t.Fatalf("outcome %+v", out)
	}
	if len(seen) != 2 || len(s.tenants) != 1 {
		t.Fatalf("checked %+v, tenants %v", seen, s.tenants)
	}
}

// Re-listing a tenant keeps what an operator changed in its entry.
func TestRelistKeepsOperatorChanges(t *testing.T) {
	s := newFake()
	handle(t, s, event("created", 67, "acme", "", 0))
	s.tenants["acme"] = json.RawMessage(`{"id":"acme","url":"https://github.com/acme","installation_id":67,"tiers":["linux-2","linux-8"],"limits":{"max_runners":20}}`)
	conf := cfg
	conf.Lookup = func(context.Context, int64) (Observed, error) {
		return Observed{Found: true, AccountID: 900, Login: "Acme-Renamed", Type: "Organization"}, nil
	}
	handleWith(t, conf, s, event("new_permissions_accepted", 67, "acme", "", time.Minute))
	c, _ := tenant(t, s, "acme")
	if c.URL != "https://github.com/Acme-Renamed" || len(c.Tiers) != 2 || c.Limits == nil || *c.Limits.MaxRunners != 20 {
		t.Fatalf("tenant %+v", c)
	}
}

// With Lookup, a delivery applies the state GitHub reports, so the order
// deliveries arrive in, and their updated_at, no longer matter.
func TestLookupIsTheTruth(t *testing.T) {
	s := newFake()
	gh := Observed{Found: true, AccountID: 900, Login: "acme", Type: "Organization"}
	conf := cfg
	conf.Lookup = func(_ context.Context, id int64) (Observed, error) {
		if id != 68 {
			return Observed{}, fmt.Errorf("installation %d", id)
		}
		return gh, nil
	}
	// suspend and unsuspend in one second, delivered the wrong way round.
	handleWith(t, conf, s, event("created", 68, "acme", "", 0))
	gh.Suspended = false
	if out := handleWith(t, conf, s, event("suspend", 68, "acme", "", time.Second)); out.Result != "listed" {
		t.Fatalf("a late suspension of an installation GitHub reports active: %+v", out)
	}
	gh.Suspended = true
	if out := handleWith(t, conf, s, event("created", 68, "acme", "", 0)); out.Result != "suspended" || len(s.tenants) != 0 {
		t.Fatalf("an old creation of an installation GitHub reports suspended: %+v", out)
	}
	gh = Observed{}
	if out := handleWith(t, conf, s, event("unsuspend", 68, "acme", "", time.Hour)); out.Result != "removed" || s.installations[68].State != "deleted" {
		t.Fatalf("an installation GitHub no longer has: %+v", out)
	}
}

// A failed lookup applies nothing, so the delivery can be retried.
func TestLookupFailure(t *testing.T) {
	s := newFake()
	conf := cfg
	conf.Lookup = func(context.Context, int64) (Observed, error) { return Observed{}, errors.New("502") }
	if _, err := Handle(context.Background(), conf, s, event("created", 69, "acme", "", 0)); err == nil {
		t.Fatal("no error")
	}
	if len(s.installations) != 0 || len(s.delivered) != 0 || len(s.tenants) != 0 {
		t.Fatal("a failed lookup applied something")
	}
}

// A login that is not a valid tenant id gets one from its account.
func TestTenantIDFallback(t *testing.T) {
	for login, want := range map[string]string{
		"acme":                                 "acme",
		"Acme-Corp":                            "acme-corp",
		"a-login-that-is-far-longer-than-32ch": "a900",
		"-dash":                                "a900",
	} {
		if got := tenantID(login, 900); got != want {
			t.Errorf("tenantID(%q) = %q, want %q", login, got, want)
		}
		if !tenantstate.ValidID(tenantID(login, 900)) {
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
	if out := handle(t, s, d); out.Result != "listed" {
		t.Fatalf("retry: %+v", out)
	}
}

// A payload missing what applying needs is an error, not a guess.
func TestMalformed(t *testing.T) {
	s := newFake()
	for _, body := range []string{
		`{`,
		`{"action":"created","installation":{"id":53,"account":{"id":900,"login":"acme","type":"Organization"}}}`,
		`{"action":"created","installation":{"id":53,"account":{"login":"acme","type":"Organization"},"updated_at":"2026-11-18T10:00:00Z"}}`,
	} {
		if _, err := Handle(context.Background(), cfg, s, sign(Delivery{ID: "m", Event: "installation", Body: []byte(body)})); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
}

// Validate refuses a configuration whose new tenants the daemon would not
// serve.
func TestValidate(t *testing.T) {
	daemon := &config.Config{URL: "https://github.com/home", AppClientID: "Iv1", AppPrivateKey: "key.pem",
		Tiers: []config.Tier{
			{Name: "linux-2", Provisioner: config.ProvisionerEC2, Schedule: &schedule.Schedule{}, EC2: &config.EC2Tier{}},
			{Name: "micro"},
		}}
	if err := cfg.Validate(daemon); err != nil {
		t.Fatalf("a good configuration: %v", err)
	}
	for name, c := range map[string]Config{
		"no secret":         {Tiers: cfg.Tiers},
		"no tiers":          {Secret: secret},
		"an unknown tier":   {Secret: secret, Tiers: []string{"nope"}},
		"a firecracker one": {Secret: secret, Tiers: []string{"micro"}},
	} {
		if err := c.Validate(daemon); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
