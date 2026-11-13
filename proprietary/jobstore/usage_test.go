package jobstore

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/firerunnerhq/firerunner/internal/jobevent"
)

// story is one job's events in a tenant: its runner launched, ran the job
// for run, and the job completed. Runner names repeat across tenants.
func story(tenant, jobID, typ string, t0 time.Time, run time.Duration) []jobevent.Event {
	r := func(kind jobevent.Kind, at time.Time) jobevent.Event {
		return jobevent.Event{Kind: kind, At: at, Tenant: tenant, Tier: "t", Runner: "runner-1", Instance: "i-" + tenant,
			InstanceType: typ, Market: "spot", Zone: "z", Path: "cold"}
	}
	j := func(kind jobevent.Kind, at time.Time) jobevent.Event {
		return jobevent.Event{Kind: kind, At: at, Tenant: tenant, Tier: "t", Runner: "runner-1", JobID: jobID,
			Repository: tenant + "/r", Workflow: tenant + "/r/.github/workflows/ci.yml@refs/heads/main", JobName: "build", EventName: "push", RunID: 9}
	}
	completed := j(jobevent.Completed, t0.Add(10*time.Second+run))
	completed.Result = "succeeded"
	assigned := j(jobevent.Assigned, t0)
	assigned.Runner = ""
	return []jobevent.Event{
		assigned, r(jobevent.Launched, t0.Add(time.Second)), r(jobevent.Served, t0.Add(5*time.Second)),
		r(jobevent.Running, t0.Add(9*time.Second)), j(jobevent.Started, t0.Add(10*time.Second)), completed,
		r(jobevent.InstanceGone, t0.Add(15*time.Second+run)),
	}
}

func usageRows(t *testing.T, s *Store) map[string]Usage {
	t.Helper()
	got := map[string]Usage{}
	for _, e := range readAll(t, s, 100) {
		var u Usage
		if err := json.Unmarshal(e.Event, &u); err != nil {
			t.Fatal(err)
		}
		got[u.EventID] = u
	}
	return got
}

func readAll(t *testing.T, s *Store, page int) []UsageEntry {
	t.Helper()
	var all []UsageEntry
	var c Cursor
	for {
		got, err := readUsageFrom(context.Background(), s.pool, c, page)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, got...)
		if len(got) < page {
			return all
		}
		if c, err = ParseCursor(got[len(got)-1].Cursor); err != nil {
			t.Fatal(err)
		}
	}
}

// Two tenants' jobs, on runners of the same name, each bill to their own
// tenant, with their own machine, for the time GitHub ran the job.
func TestUsageAtCompletion(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	events := append(story("acme", "101", "c8a.large", t0, 90500*time.Millisecond), story("", "202", "m8a.xlarge", t0, 30*time.Second)...)
	if err := s.writeEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	got := usageRows(t, s)
	if len(got) != 2 {
		t.Fatalf("%d usage events, want 2: %v", len(got), got)
	}
	a, b := got["job/101"], got["job/202"]
	if a.Tenant != "acme" || a.InstanceType != "c8a.large" || a.DurationMS != 90500 || a.Result != "succeeded" ||
		a.StartSource != "github" || a.Repository != "acme/r" || a.Supply != "cold" || a.Schema != UsageSchema || a.Kind != "job" {
		t.Fatalf("acme's job: %+v", a)
	}
	if b.Tenant != "" || b.InstanceType != "m8a.xlarge" || b.DurationMS != 30000 {
		t.Fatalf("the install's job: %+v", b)
	}
	if a.StartedAt == nil || !a.StartedAt.Equal(t0.Add(10*time.Second)) || !a.CompletedAt.Equal(t0.Add(100500*time.Millisecond)) {
		t.Fatalf("acme's times: %v to %v", a.StartedAt, a.CompletedAt)
	}
	var runners int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runners WHERE runner = 'runner-1'`).Scan(&runners); err != nil || runners != 2 {
		t.Fatalf("%d runner rows named runner-1, want one per tenant (%v)", runners, err)
	}
	var tenant string
	if err := s.pool.QueryRow(ctx, `SELECT tenant FROM job_timeline WHERE job_id = '101'`).Scan(&tenant); err != nil || tenant != "acme" {
		t.Fatalf("timeline tenant %q, %v", tenant, err)
	}
}

// A completion delivered again, and every event replayed, add no usage
// and change none: an event is written once, under the job's id.
func TestUsageIsWrittenOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	events := story("acme", "101", "c8a.large", t0, time.Minute)
	if err := s.writeEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	first := readAll(t, s, 10)
	late := events[5] // the completion, delivered again later
	late.At = t0.Add(time.Hour)
	if err := s.writeEvents(ctx, []jobevent.Event{late}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeEvents(ctx, events); err != nil { // a replay
		t.Fatal(err)
	}
	again := readAll(t, s, 10)
	if len(first) != 1 || len(again) != 1 || string(first[0].Event) != string(again[0].Event) || first[0].Cursor != again[0].Cursor {
		t.Fatalf("before %s, after %s", first, again)
	}
}

// A consumer that reads some events twice, as at-least-once delivery
// does, still totals each job once by deduping on event_id.
func TestUsageConsumerTotalsOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var want int64
	for i, id := range []string{"1", "2", "3", "4", "5"} {
		run := time.Duration(i+1) * time.Minute
		want += run.Milliseconds()
		if err := s.writeEvents(ctx, story("acme", id, "c8a.large", t0, run)); err != nil {
			t.Fatal(err)
		}
	}
	// Pages of two, with the second page read again as if its
	// acknowledgement were lost.
	var delivered []UsageEntry
	var c Cursor
	for page := 0; ; page++ {
		got, err := readUsageFrom(ctx, s.pool, c, 2)
		if err != nil {
			t.Fatal(err)
		}
		delivered = append(delivered, got...)
		if len(got) == 0 {
			break
		}
		if page == 1 {
			continue // cursor not advanced: the page comes again
		}
		if c, err = ParseCursor(got[len(got)-1].Cursor); err != nil {
			t.Fatal(err)
		}
		if page > 10 {
			t.Fatal("never reached the end")
		}
	}
	seen := map[string]bool{}
	var total int64
	for _, e := range delivered {
		var u Usage
		if err := json.Unmarshal(e.Event, &u); err != nil {
			t.Fatal(err)
		}
		if !seen[u.EventID] {
			seen[u.EventID] = true
			total += u.DurationMS
		}
	}
	if len(delivered) <= 5 || len(seen) != 5 || total != want {
		t.Fatalf("delivered %d, distinct %d, total %d ms, want 5 distinct, %d ms", len(delivered), len(seen), total, want)
	}
}

// A transaction that took its row before another but commits after it is
// not skipped: a reader sees neither until the earlier one ends.
func TestUsageReadWaitsForAnOpenTransaction(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	conn, err := pgx.Connect(ctx, os.Getenv("FR_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	slow, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Rollback(ctx)
	// The slow transaction takes its id first, and its usage row's
	// sequence number last.
	b := &pgx.Batch{}
	for _, e := range story("acme", "early", "c8a.large", t0, time.Minute) {
		queue(b, e)
	}
	if err := slow.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	// Another tenant's, as the same runner's row would wait on the open
	// transaction's lock.
	if err := s.writeEvents(ctx, story("beta", "late", "c8a.large", t0, time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := slow.Exec(ctx, insertUsage, "early"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, 10); len(got) != 0 {
		t.Fatalf("read %d event(s) while an earlier transaction is open: a cursor past them would skip its row", len(got))
	}
	if err := slow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// One a page, so each read starts from the cursor the last returned.
	got := usageIDs(t, readAll(t, s, 1))
	if len(got) != 2 || got[0] != "job/early" || got[1] != "job/late" {
		t.Fatalf("read %v, want the early transaction's event, then the late one's", got)
	}
}

func usageIDs(t *testing.T, entries []UsageEntry) []string {
	t.Helper()
	var ids []string
	for _, e := range entries {
		var u Usage
		if err := json.Unmarshal(e.Event, &u); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, u.EventID)
	}
	return ids
}

// A job nothing ran (cancelled while queued) uses nothing; one whose start
// from GitHub was lost bills from its runner's report of taking it.
func TestUsageWithoutARunnerOrAStart(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cancelled := []jobevent.Event{
		{Kind: jobevent.Assigned, At: t0, Tenant: "acme", JobID: "queued"},
		{Kind: jobevent.Completed, At: t0.Add(time.Minute), Tenant: "acme", JobID: "queued", Result: "canceled"},
	}
	var lost []jobevent.Event
	for _, e := range story("acme", "lost", "c8a.large", t0, time.Minute) {
		if e.Kind != jobevent.Started {
			lost = append(lost, e)
		}
	}
	if err := s.writeEvents(ctx, append(cancelled, lost...)); err != nil {
		t.Fatal(err)
	}
	got := usageRows(t, s)
	u, ok := got["job/lost"]
	if len(got) != 1 || !ok {
		t.Fatalf("usage %v: want the lost start's job alone", got)
	}
	if u.StartSource != "runner" || u.DurationMS != 61000 {
		t.Fatalf("start %q, %d ms: want the runner's, from 9 s to 70 s", u.StartSource, u.DurationMS)
	}
}

// A store set up before tenants keeps its rows: they become the install's.
func TestTenantMigrationKeepsRows(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, m := range []string{"migrations/001_jobs.sql", "migrations/002_supply_seconds.sql"} {
			if err := applyMigration(ctx, tx, m); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO runners (runner, instance_type) VALUES ('runner-1', 'c8a.large');
			INSERT INTO jobs (job_id, runner) VALUES ('1', 'runner-1')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setUp(ctx); err != nil {
		t.Fatal(err)
	}
	var typ, tenant string
	if err := s.pool.QueryRow(ctx, `SELECT instance_type, tenant FROM job_timeline WHERE job_id = '1'`).Scan(&typ, &tenant); err != nil || typ != "c8a.large" || tenant != "" {
		t.Fatalf("after the migration: %q %q %v", typ, tenant, err)
	}
}

// The schema that documents the event names every field the store writes,
// requires each, and Usage reads them all.
func TestUsageMatchesItsSchema(t *testing.T) {
	s := open(t)
	if err := s.writeEvents(context.Background(), story("acme", "1", "c8a.large", time.Now(), time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, s, 10)
	if len(got) != 1 {
		t.Fatalf("%d events", len(got))
	}
	var written map[string]any
	if err := json.Unmarshal(got[0].Event, &written); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("usage-event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	read, err := json.Marshal(Usage{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(read, &fields); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != len(written) || len(schema.Properties) != len(written) || len(fields) != len(written) {
		t.Fatalf("written %d fields, schema requires %d and describes %d, Usage reads %d", len(written), len(schema.Required), len(schema.Properties), len(fields))
	}
	for _, k := range schema.Required {
		_, w := written[k]
		_, p := schema.Properties[k]
		_, f := fields[k]
		if !w || !p || !f {
			t.Errorf("%s: written %v, described %v, read %v", k, w, p, f)
		}
	}
	if written["schema"] != float64(UsageSchema) {
		t.Errorf("schema %v, want %d", written["schema"], UsageSchema)
	}
}

// Before any daemon set the store up, a read says so.
func TestUsageReaderBeforeSetUp(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	r, err := OpenUsage(os.Getenv("FR_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Read(ctx, Cursor{}, 10); err == nil || !strings.Contains(err.Error(), "no usage events table") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseCursor(t *testing.T) {
	for in, want := range map[string]Cursor{"": {}, "0.0": {}, "812.17": {812, 17}} {
		got, err := ParseCursor(in)
		if err != nil || got != want {
			t.Errorf("ParseCursor(%q) = %v, %v", in, got, err)
		}
		if in != "" && got.String() != in {
			t.Errorf("%v.String() = %q", got, got.String())
		}
	}
	for _, in := range []string{"5", "a.b", "1.-1", "-1.2", "1.2.3"} {
		if _, err := ParseCursor(in); err == nil {
			t.Errorf("ParseCursor(%q) succeeded", in)
		}
	}
}
