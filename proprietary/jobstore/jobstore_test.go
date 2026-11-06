package jobstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/firerunnerhq/firerunner/internal/jobevent"
)

func TestRecordNeverBlocks(t *testing.T) {
	s := &Store{queue: make(chan jobevent.Event, 1)}
	s.Record(jobevent.Event{})
	s.Record(jobevent.Event{}) // no writer is reading: this must not wait
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d", s.Dropped())
	}
}

// fake is a store with a scripted database.
func fake(t *testing.T, write func(context.Context, []jobevent.Event) error) *Store {
	t.Helper()
	wait := retryWait
	retryWait = time.Millisecond
	t.Cleanup(func() { retryWait = wait })
	s := newStore(nil)
	s.write = write
	s.prepare = func(context.Context) error { return nil }
	return s
}

func ev(id string) jobevent.Event { return jobevent.Event{JobID: id} }

// One row the database refuses costs that row, and the rest of its batch lands.
func TestFlushIsolatesARejectedRow(t *testing.T) {
	var wrote []string
	s := fake(t, func(_ context.Context, b []jobevent.Event) error {
		for _, e := range b {
			if e.JobID == "bad" {
				return &pgconn.PgError{Code: "23514"}
			}
		}
		for _, e := range b {
			wrote = append(wrote, e.JobID)
		}
		return nil
	})
	s.flush([]jobevent.Event{ev("a"), ev("bad"), ev("b"), ev("c")})
	if len(wrote) != 3 || s.Dropped() != 1 {
		t.Fatalf("wrote %v, dropped %d", wrote, s.Dropped())
	}
}

// A database that is away is waited for: nothing is lost.
func TestFlushRetriesAnOutage(t *testing.T) {
	calls := 0
	s := fake(t, func(context.Context, []jobevent.Event) error {
		calls++
		if calls < 3 {
			return errors.New("connection refused")
		}
		return nil
	})
	s.flush([]jobevent.Event{ev("a")})
	if calls != 3 || s.Dropped() != 0 {
		t.Fatalf("calls %d, dropped %d", calls, s.Dropped())
	}
}

// Set-up is retried too, and events wait for it.
func TestEventsWaitForSetUp(t *testing.T) {
	got := make(chan string, 1)
	s := fake(t, func(_ context.Context, b []jobevent.Event) error {
		got <- b[0].JobID
		return nil
	})
	var tries atomic.Int32
	s.prepare = func(context.Context) error {
		if tries.Add(1) < 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	s.Record(ev("a"))
	go s.run()
	defer s.Close(context.Background())
	select {
	case id := <-got:
		if id != "a" || tries.Load() != 3 {
			t.Fatalf("id %q, tries %d", id, tries.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event never landed")
	}
}

func TestCloseWritesWhatIsLeft(t *testing.T) {
	var wrote int
	s := fake(t, func(_ context.Context, b []jobevent.Event) error {
		wrote += len(b)
		return nil
	})
	go s.run()
	s.Record(ev("a"))
	s.Record(ev("b"))
	s.Close(context.Background())
	s.Close(context.Background()) // a second close is harmless
	if wrote != 2 {
		t.Fatalf("wrote %d", wrote)
	}
}

func TestCloseCountsWhatItLoses(t *testing.T) {
	s := fake(t, func(context.Context, []jobevent.Event) error { return errors.New("connection refused") })
	s.Record(ev("a"))
	close(s.stop)
	s.run()
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d", s.Dropped())
	}
}

// Connection errors name hosts and users, so they stay out of the log.
func TestDescribeHidesTheConnection(t *testing.T) {
	err := errors.New("failed to connect to user=u database=d host=h")
	if got := describe(err); strings.Contains(got, "user=") {
		t.Fatalf("describe = %q", got)
	}
}

func TestNewRefusesABadURLWithoutEchoingIt(t *testing.T) {
	_, err := New("postgres://u:secret@host:notaport/db", nil)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

// open connects to FR_TEST_POSTGRES_URL, a throwaway database, with a clean
// schema. CI's test job provides one.
func open(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FR_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("FR_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	s, err := New(dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(ctx) })
	if err := s.setUp(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func job(kind jobevent.Kind, at time.Time) jobevent.Event {
	return jobevent.Event{Kind: kind, At: at, Tier: "t", JobID: "job-1", Repository: "o/r", Workflow: "o/r/ci.yml@main", JobName: "build",
		EventName: "push", RunID: 9, QueuedAt: at.Add(-time.Minute)}
}

func runner(kind jobevent.Kind, at time.Time) jobevent.Event {
	return jobevent.Event{Kind: kind, At: at, Tier: "t", Runner: "runner-1", Instance: "i-1", InstanceType: "c8a.large", Market: "spot", Zone: "z", Path: "cold"}
}

// The whole story, arriving out of order and twice, reads the same.
func TestTimeline(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	started := job(jobevent.Started, t0.Add(20*time.Second))
	started.Runner = "runner-1"
	completed := job(jobevent.Completed, t0.Add(80*time.Second))
	completed.Runner, completed.Result = "runner-1", "succeeded"
	exited := runner(jobevent.Exited, t0.Add(81*time.Second))
	exited.ExitCode = 0
	gone := runner(jobevent.InstanceGone, t0.Add(82*time.Second))
	gone.Reason = "Client.UserInitiatedShutdown"
	events := []jobevent.Event{
		completed, started, exited, gone,
		runner(jobevent.Listening, t0.Add(15*time.Second)), runner(jobevent.Running, t0.Add(16*time.Second)),
		runner(jobevent.Launched, t0.Add(2*time.Second)), runner(jobevent.Served, t0.Add(12*time.Second)),
		job(jobevent.Assigned, t0.Add(time.Second)),
	}
	if err := s.writeEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := s.writeEvents(ctx, events[:2]); err != nil { // a redelivery
		t.Fatal(err)
	}

	var repo, typ, result, reason string
	var boot, runnerS, pickup, queue, seconds float64
	err := s.pool.QueryRow(ctx, `SELECT repository, instance_type, result, gone_reason, boot_seconds, runner_seconds, pickup_seconds, job_seconds,
		EXTRACT(EPOCH FROM queued_at) FROM job_timeline WHERE job_id = 'job-1'`).Scan(&repo, &typ, &result, &reason, &boot, &runnerS, &pickup, &seconds, &queue)
	if err != nil {
		t.Fatal(err)
	}
	if repo != "o/r" || typ != "c8a.large" || result != "succeeded" || reason == "" {
		t.Fatalf("got %q %q %q %q", repo, typ, result, reason)
	}
	if boot != 10 || runnerS != 3 || pickup != 1 || seconds != 60 {
		t.Fatalf("boot=%v runner=%v pickup=%v job=%v", boot, runnerS, pickup, seconds)
	}
	// An event that arrives early is not undone by the ones after it.
	var kept bool
	err = s.pool.QueryRow(ctx, `SELECT launched_at IS NOT NULL AND served_at IS NOT NULL AND listening_at IS NOT NULL
		AND running_at IS NOT NULL AND exited_at IS NOT NULL AND exit_code = 0 AND gone_at IS NOT NULL FROM runners`).Scan(&kept)
	if err != nil || !kept {
		t.Fatalf("a runner lost a stage: %v, %v", kept, err)
	}
	err = s.pool.QueryRow(ctx, `SELECT assigned_at IS NOT NULL AND started_at IS NOT NULL AND completed_at IS NOT NULL FROM jobs`).Scan(&kept)
	if err != nil || !kept {
		t.Fatalf("a job lost a stage: %v, %v", kept, err)
	}
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("jobs = %d, %v", rows, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM job_events`).Scan(&rows); err != nil || rows != len(events)+2 {
		t.Fatalf("events = %d, %v", rows, err)
	}
}

func TestWriterFlushes(t *testing.T) {
	s := open(t)
	s.Record(job(jobevent.Assigned, time.Now()))
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the writer never wrote the event")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPartitionsAreCreatedAhead(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, monthsAhead, 0)
	e := jobevent.Event{Kind: jobevent.Launched, At: future, Runner: "r"}
	if err := s.writeEvents(ctx, []jobevent.Event{e}); err != nil {
		t.Fatalf("an event %d months ahead: %v", monthsAhead, err)
	}
	var partition string
	if err := s.pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM job_events`).Scan(&partition); err != nil {
		t.Fatal(err)
	}
	if partition == "job_events_default" {
		t.Fatal("the month ahead has no partition")
	}
}

// An event outside every month, or holding NUL, must not fail its batch.
func TestStrangeEventsAreKept(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := jobevent.Event{Kind: jobevent.Assigned, At: time.Unix(0, 0), JobID: "old", JobName: "a\x00b", Labels: []string{"c\x00"}}
	if err := s.writeEvents(ctx, []jobevent.Event{old}); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT job_name FROM jobs WHERE job_id = 'old'`).Scan(&name); err != nil || name != "ab" {
		t.Fatalf("%q, %v", name, err)
	}
}

// Shards starting together on an empty database all succeed.
func TestConcurrentSetUp(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for range cap(errs) {
		go func() { errs <- s.setUp(ctx) }()
	}
	for range cap(errs) {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// A session in another time zone still creates UTC months.
func TestPartitionBoundsAreUTC(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 3, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, sql := range []string{
		`SET LOCAL TIME ZONE 'America/Los_Angeles'`,
		`DROP TABLE job_events_default`,
		`DROP TABLE job_events_` + first.Format("2006_01"),
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensurePartitions(ctx, tx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_events (at, kind) VALUES ($1, 'x')`, first); err != nil {
		t.Fatalf("03:00 UTC on the 1st found no partition: %v", err)
	}
}
