package guardduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

const mining = "CryptoCurrency:EC2/BitcoinTool.B!DNS"

var t0 = time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)

// clock is a test's time, moved by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// rig is a poller on a fake GuardDuty and a file flag store, serving the
// install "inst" and its tenant "acme".
type rig struct {
	gd      *fakeGD
	store   tenantstate.FlagStore
	clock   *clock
	flagged []Flagged
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := tenantstate.OpenFlags(filepath.Join(t.TempDir(), "tenant-state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{gd: &fakeGD{detectors: []string{"d1"}}, store: store, clock: &clock{t: t0}}
}

// poller is a fresh poller on the rig's store, as after a restart.
func (r *rig) poller() *Poller {
	return &Poller{
		API: r.gd, Store: r.store, Tenant: Owners("inst", []string{"acme"}),
		MinSeverity: DefaultMinSeverity, now: r.clock.now,
		Account: func(context.Context) (string, error) { return ownAccount, nil },
		OnFlag:  func(f Flagged) { r.flagged = append(r.flagged, f) },
	}
}

func tags(owner string) map[string]string {
	return map[string]string{"firerunner:owner": owner, "firerunner:tier": "small", "firerunner:runner": "r1", "Name": "x"}
}

func mustPoll(t *testing.T, p *Poller) {
	t.Helper()
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A finding on a served tenant's machine flags it once, however often it is
// read; findings on other owners', below the severity, or archived flag
// nothing. Nothing here can suspend: the poller holds no state store.
func TestPollFlagsTheTenantOnce(t *testing.T) {
	r := newRig(t)
	at := t0.Add(-4 * time.Minute)
	r.gd.add("f-acme", mining, 8, at, false, tags("inst/acme"))
	r.gd.add("f-other", mining, 8, at, false, tags("someone-else/acme"))
	r.gd.add("f-unknown", mining, 8, at, false, tags("inst/stranger"))
	r.gd.add("f-low", "Recon:EC2/PortProbeUnprotectedPort", 2, at, false, tags("inst/acme"))
	r.gd.add("f-archived", mining, 8, at, true, tags("inst/acme"))
	p := r.poller()
	mustPoll(t, p)
	mustPoll(t, p)
	r.clock.add(DefaultInterval)
	mustPoll(t, p)

	if len(r.flagged) != 1 || r.flagged[0].Tenant != "acme" || r.flagged[0].ID != "f-acme" || r.flagged[0].Runner != "r1" {
		t.Fatalf("flagged %+v, want f-acme once", r.flagged)
	}
	want := tenantstate.Flag{Type: mining, Tier: "small", Instance: "i-f-acme", Finding: "f-acme", Severity: 8, Count: 1, First: at, Last: at}
	if got := p.Flags(); len(got) != 1 || got["acme"] != want {
		t.Fatalf("Flags() = %+v, want acme: %+v", got, want)
	}
	stored, _, err := r.store.LoadFlags(context.Background())
	if err != nil || len(stored) != 1 || stored["acme"] != want {
		t.Fatalf("stored %+v, %v", stored, err)
	}
	st := p.Status()
	if st.Detectors != 1 || !st.LastPoll.Equal(t0.Add(DefaultInterval)) || st.LastError != "" || st.MinSeverity != DefaultMinSeverity {
		t.Fatalf("status %+v", st)
	}
}

// The install's own machines flag the install; a finding GuardDuty updates
// again (the activity recurred) adds to the flag once, keeping the first time
// and the highest severity.
func TestPollInstallAndRecurrence(t *testing.T) {
	r := newRig(t)
	r.gd.add("f1", mining, 8, t0.Add(-time.Minute), false, tags("inst"))
	p := r.poller()
	mustPoll(t, p)
	r.gd.update("f1", t0.Add(time.Minute))
	r.gd.add("f2", "Backdoor:EC2/C&CActivity.B!DNS", 5, t0.Add(2*time.Minute), false, tags("inst"))
	r.clock.add(3 * time.Minute)
	mustPoll(t, p)
	mustPoll(t, p)
	f := p.Flags()[tenantstate.Install]
	if f.Count != 3 || f.Severity != 8 || !f.First.Equal(t0.Add(-time.Minute)) || !f.Last.Equal(t0.Add(2*time.Minute)) ||
		f.Finding != "f2" || f.Type != "Backdoor:EC2/C&CActivity.B!DNS" {
		t.Fatalf("install flag %+v: want three findings, first at -1m, last f2 at +2m, severity 8", f)
	}
	if len(r.flagged) != 3 {
		t.Fatalf("OnFlag heard %d, want 3", len(r.flagged))
	}
}

// The severity is checked on each finding as well as in the criteria, which
// take only whole numbers: 4.2 is below a minimum of 4.5.
func TestPollMinSeverity(t *testing.T) {
	r := newRig(t)
	r.gd.add("f-42", mining, 4.2, t0, false, tags("inst/acme"))
	r.gd.add("f-45", mining, 4.5, t0, false, tags("inst/acme"))
	p := r.poller()
	p.MinSeverity = 4.5
	mustPoll(t, p)
	if len(r.flagged) != 1 || r.flagged[0].ID != "f-45" {
		t.Fatalf("flagged %+v, want only f-45", r.flagged)
	}
}

// After a restart, the cursor keeps a finding already flagged from flagging
// again, even once an operator has cleared the flag; a new one still flags.
func TestCursorSurvivesARestart(t *testing.T) {
	r := newRig(t)
	r.gd.add("f1", mining, 8, t0.Add(-time.Minute), false, tags("inst/acme"))
	mustPoll(t, r.poller())
	if err := r.store.DeleteFlag(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	r.clock.add(time.Minute)
	p := r.poller()
	mustPoll(t, p)
	if len(r.flagged) != 1 || len(p.Flags()) != 0 {
		t.Fatalf("after a clear and a restart: flagged %d times, flags %v", len(r.flagged), p.Flags())
	}
	r.gd.add("f2", mining, 8, t0.Add(time.Minute), false, tags("inst/acme"))
	r.clock.add(time.Minute)
	p = r.poller()
	mustPoll(t, p)
	if f := p.Flags()["acme"]; len(r.flagged) != 2 || f.Count != 1 || f.Finding != "f2" {
		t.Fatalf("a new finding after a restart: flagged %d, flag %+v", len(r.flagged), f)
	}
	_, cur, err := r.store.LoadFlags(context.Background())
	if err != nil || !cur.Since.Equal(r.clock.now().Add(-Settle)) {
		t.Fatalf("cursor %+v, %v: want Settle before the last poll", cur, err)
	}
}

// Each finding is checked again as it is read, so a criterion GuardDuty
// dropped never flags: archived, below the severity, older than the cursor,
// on another kind of resource, or on an instance without an owner tag.
func TestChecksHoldWithoutTheCriteria(t *testing.T) {
	r := newRig(t)
	r.gd.ignoreCriteria = true
	r.gd.add("archived", mining, 8, t0, true, tags("inst/acme"))
	r.gd.add("low", mining, 3.9, t0, false, tags("inst/acme"))
	r.gd.add("old", mining, 8, t0.Add(-FirstLookback-time.Minute), false, tags("inst/acme"))
	r.gd.add("untagged", mining, 8, t0, false, map[string]string{"Name": "x"})
	r.gd.add("bucket", mining, 8, t0, false, tags("inst/acme"))
	r.gd.findings[len(r.gd.findings)-1].Resource.ResourceType = aws.String("S3Bucket")
	r.gd.add("good", mining, 8, t0, false, tags("inst/acme"))
	mustPoll(t, r.poller())
	if len(r.flagged) != 1 || r.flagged[0].ID != "good" {
		t.Fatalf("flagged %+v, want only the good one", r.flagged)
	}
	// List, which tenant findings shows as is, drops them too.
	found, _, err := List(context.Background(), r.gd, Query{Since: t0.Add(-FirstLookback), MinSeverity: DefaultMinSeverity, Account: ownAccount})
	if err != nil || len(found) != 1 || found[0].ID != "good" {
		t.Fatalf("List = %+v, %v; want only the good one", found, err)
	}
}

// The first read of an empty store looks back FirstLookback, no further.
func TestFirstReadLooksBack(t *testing.T) {
	r := newRig(t)
	r.gd.add("old", mining, 8, t0.Add(-FirstLookback-time.Minute), false, tags("inst/acme"))
	r.gd.add("recent", mining, 8, t0.Add(-FirstLookback+time.Minute), false, tags("inst/acme"))
	mustPoll(t, r.poller())
	if len(r.flagged) != 1 || r.flagged[0].ID != "recent" {
		t.Fatalf("flagged %+v, want only the one within FirstLookback", r.flagged)
	}
}

// failingStore fails its flag writes while fail is set, those for failFor
// only, and its cursor writes while failCursor is.
type failingStore struct {
	tenantstate.FlagStore
	fail       bool
	failFor    string
	failCursor bool
	cursors    int
}

func (f *failingStore) PutFlag(ctx context.Context, id string, fl tenantstate.Flag) error {
	if f.fail || (f.failFor != "" && id == f.failFor) {
		return errors.New("ThrottlingException")
	}
	return f.FlagStore.PutFlag(ctx, id, fl)
}

func (f *failingStore) PutCursor(ctx context.Context, c tenantstate.Cursor) error {
	f.cursors++
	if f.failCursor {
		return errors.New("ThrottlingException")
	}
	return f.FlagStore.PutCursor(ctx, c)
}

// A cursor that cannot be written, again and again, never makes a finding
// count again: the poller keeps its own. Nor does a flag written for one
// tenant when another's write fails; the other's finding is heard once its
// flag is written.
func TestFailedWritesNeverCountTwice(t *testing.T) {
	r := newRig(t)
	// Flags are written in id order: _install's first, then acme's.
	fs := &failingStore{FlagStore: r.store, failCursor: true, failFor: "acme"}
	r.store = fs
	r.gd.add("f-acme", mining, 8, t0, false, tags("inst/acme"))
	r.gd.add("f-inst", mining, 8, t0, false, tags("inst"))
	p := r.poller()
	for range 3 {
		r.clock.add(Settle)
		if err := p.Poll(context.Background()); err == nil {
			t.Fatal("a failed write: no error")
		}
	}
	fl, _, _ := r.store.LoadFlags(context.Background())
	if fl[tenantstate.Install].Count != 1 || len(r.flagged) != 1 || r.flagged[0].ID != "f-inst" {
		t.Fatalf("after failed writes: install %+v, heard %+v", fl[tenantstate.Install], r.flagged)
	}
	fs.failFor = ""
	r.clock.add(Settle)
	_ = p.Poll(context.Background())
	fs.failCursor = false
	r.clock.add(Settle)
	mustPoll(t, p)
	fl, _, _ = r.store.LoadFlags(context.Background())
	if fl["acme"].Count != 1 || fl[tenantstate.Install].Count != 1 || len(r.flagged) != 2 {
		t.Fatalf("after the writes succeed: %+v, heard %d", fl, len(r.flagged))
	}
}

// More findings in the window than the cursor holds count once each, poll
// after poll: the cursor moves past those it lets go.
func TestBurstPastTheCap(t *testing.T) {
	r := newRig(t)
	for i := range maxSeen + 5 {
		r.gd.add(fmt.Sprintf("f%03d", i), mining, 8, t0.Add(time.Duration(i)*time.Second), false, tags("inst/acme"))
	}
	r.clock.add(time.Minute)
	for range 4 {
		mustPoll(t, r.poller()) // a fresh poller: the stored cursor alone
		r.clock.add(DefaultInterval)
	}
	fl, cur, _ := r.store.LoadFlags(context.Background())
	if fl["acme"].Count != maxSeen+5 || len(r.flagged) != maxSeen+5 || len(cur.Seen) > maxSeen {
		t.Fatalf("count %d, heard %d, cursor holds %d", fl["acme"].Count, len(r.flagged), len(cur.Seen))
	}
}

// A finding GuardDuty lists only after a poll that should have seen it, but
// within Settle, still flags.
func TestLateListing(t *testing.T) {
	r := newRig(t)
	p := r.poller()
	mustPoll(t, p)
	r.clock.add(DefaultInterval)
	r.gd.add("late", mining, 8, t0.Add(-time.Minute), false, tags("inst/acme"))
	mustPoll(t, p)
	if len(r.flagged) != 1 || r.flagged[0].ID != "late" {
		t.Fatalf("flagged %+v, want the late one", r.flagged)
	}
}

// Findings in another account, as a GuardDuty administrator lists its
// members', never flag, whatever their tags.
func TestOtherAccountIgnored(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		r := newRig(t)
		r.gd.ignoreCriteria = ignore
		r.gd.add("member", mining, 8, t0, false, tags("inst/acme"))
		r.gd.findings[0].AccountId = aws.String("210987654321")
		r.gd.add("own", mining, 8, t0, false, tags("inst/acme"))
		mustPoll(t, r.poller())
		if len(r.flagged) != 1 || r.flagged[0].ID != "own" {
			t.Fatalf("criteria ignored %v: flagged %+v", ignore, r.flagged)
		}
		// GuardDuty is asked for this account's alone, too.
		if got := r.gd.criteria["accountId"].Equals; len(got) != 1 || got[0] != ownAccount {
			t.Fatalf("the accountId criterion = %v", got)
		}
	}
}

// The account is asked until it answers; until then nothing is read.
func TestAccountRetried(t *testing.T) {
	r := newRig(t)
	r.gd.add("f1", mining, 8, t0, false, tags("inst/acme"))
	p := r.poller()
	asked := 0
	p.Account = func(context.Context) (string, error) {
		asked++
		if asked == 1 {
			return "", errors.New("ExpiredToken")
		}
		return ownAccount, nil
	}
	if err := p.Poll(context.Background()); err == nil || len(r.flagged) != 0 {
		t.Fatalf("without the account: %v, flagged %d", err, len(r.flagged))
	}
	mustPoll(t, p)
	mustPoll(t, p)
	if asked != 2 || len(r.flagged) != 1 {
		t.Fatalf("asked %d times, flagged %d", asked, len(r.flagged))
	}
}

// A full cursor, with ids and times as GuardDuty's, fits one SSM parameter.
func TestCursorFitsOneParameter(t *testing.T) {
	cur := tenantstate.Cursor{Since: t0.Add(123456789 * time.Nanosecond), Seen: map[string]time.Time{}}
	for i := range maxSeen {
		cur.Seen[fmt.Sprintf("%032x", i)] = t0.Add(time.Duration(i)*time.Millisecond + 987654321)
	}
	b, err := json.Marshal(cur)
	if err != nil || len(b) >= 4096 {
		t.Fatalf("%d bytes, %v: an SSM String parameter holds 4096", len(b), err)
	}
}

// A flag that cannot be written leaves the cursor where it was, so the next
// poll flags the tenant; a failed read is reported and keeps the flags.
func TestPollFailures(t *testing.T) {
	r := newRig(t)
	fs := &failingStore{FlagStore: r.store, fail: true}
	r.store = fs
	r.gd.add("f1", mining, 8, t0, false, tags("inst/acme"))
	p := r.poller()
	if err := p.Poll(context.Background()); err == nil || fs.cursors != 0 {
		t.Fatalf("a failed flag write: err %v, %d cursor writes", err, fs.cursors)
	}
	if st := p.Status(); !strings.Contains(st.LastError, "ThrottlingException") {
		t.Fatalf("status %+v", st)
	}
	fs.fail = false
	r.clock.add(time.Minute)
	mustPoll(t, p)
	if p.Flags()["acme"].Count != 1 {
		t.Fatalf("after the retry: %+v", p.Flags())
	}

	r.gd.err = errors.New("AccessDeniedException")
	r.clock.add(time.Minute)
	if err := p.Poll(context.Background()); err == nil {
		t.Fatal("a failed list: no error")
	}
	st := p.Status()
	if !strings.Contains(st.LastError, "AccessDenied") || !st.LastErrorAt.After(st.LastPoll) || p.Flags()["acme"].Count != 1 {
		t.Fatalf("after a failed list: %+v, flags %v", st, p.Flags())
	}
}

// With no detector in the region, the poll succeeds and says so.
func TestNoDetector(t *testing.T) {
	r := newRig(t)
	r.gd.detectors = nil
	p := r.poller()
	mustPoll(t, p)
	if st := p.Status(); st.Detectors != 0 || st.LastPoll.IsZero() {
		t.Fatalf("status %+v", st)
	}
}

// Many findings are fetched in batches GetFindings accepts.
func TestManyFindings(t *testing.T) {
	r := newRig(t)
	for i := range 120 {
		r.gd.add(fmt.Sprintf("f%03d", i), mining, 8, t0.Add(time.Duration(i)*time.Second-time.Hour), false, tags("inst/acme"))
	}
	p := r.poller()
	mustPoll(t, p)
	if f := p.Flags()["acme"]; f.Count != 120 || f.Finding != "f119" || r.gd.gets != 3 {
		t.Fatalf("flag %+v after %d GetFindings calls", f, r.gd.gets)
	}
}

// The cursor moves to Settle before now and never back, and keeps only the
// findings seen since, at most maxSeen of the latest.
func TestAdvance(t *testing.T) {
	cur := tenantstate.Cursor{Since: t0, Seen: map[string]time.Time{"old": t0.Add(-time.Second), "new": t0}}
	got := advance(cur, t0.Add(Settle-time.Minute))
	if !got.Since.Equal(t0) || len(got.Seen) != 1 || got.Seen["new"].IsZero() {
		t.Fatalf("advance kept %+v", got)
	}
	got = advance(got, t0.Add(Settle+time.Minute))
	if !got.Since.Equal(t0.Add(time.Minute)) || len(got.Seen) != 0 {
		t.Fatalf("advance moved to %+v", got)
	}
	seen := map[string]time.Time{}
	for i := range maxSeen + 5 {
		seen[fmt.Sprint(i)] = t0.Add(time.Duration(i) * time.Second)
	}
	got = advance(tenantstate.Cursor{Since: t0, Seen: seen}, t0)
	if len(got.Seen) != maxSeen || !got.Seen["0"].IsZero() || got.Seen[fmt.Sprint(maxSeen+4)].IsZero() {
		t.Fatalf("capped to %d, oldest kept %v", len(got.Seen), !got.Seen["0"].IsZero())
	}
}

func TestOwners(t *testing.T) {
	served := Owners("inst", []string{"acme"})
	anyID := Owners("inst", nil)
	for _, c := range []struct {
		owner string
		f     func(string) (string, bool)
		want  string
	}{
		{"inst", served, tenantstate.Install},
		{"inst/acme", served, "acme"},
		{"inst/other", served, ""},
		{"inst/other", anyID, "other"},
		{"inst/_install", anyID, ""},
		{"inst/Bad ID", anyID, ""},
		{"inst2/acme", served, ""},
		{"insta", anyID, ""},
		{"", anyID, ""},
	} {
		got, ok := c.f(c.owner)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("owner %q: %q, %v; want %q", c.owner, got, ok, c.want)
		}
	}
}

// Run polls at once, then on every tick, until its context ends.
func TestRunPollsEachTick(t *testing.T) {
	r := newRig(t)
	p := r.poller()
	tick := make(chan time.Time)
	p.tick = tick
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitPolled := func(at time.Time) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for !p.Status().LastPoll.Equal(at) {
			select {
			case <-deadline:
				t.Fatalf("no poll at %v: %+v", at, p.Status())
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitPolled(t0)
	r.clock.add(time.Minute)
	tick <- time.Time{}
	waitPolled(t0.Add(time.Minute))
	cancel()
	<-done
}
