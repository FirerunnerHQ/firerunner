package jobstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	s, err := Open(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(ctx) })
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
	if err := s.write(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, events[:2]); err != nil { // a redelivery
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
	future := time.Now().UTC().AddDate(0, monthsAhead, 0)
	e := jobevent.Event{Kind: jobevent.Launched, At: future, Runner: "r"}
	if err := s.write(ctx, []jobevent.Event{e}); err != nil {
		t.Fatalf("an event %d months ahead: %v", monthsAhead, err)
	}
}
