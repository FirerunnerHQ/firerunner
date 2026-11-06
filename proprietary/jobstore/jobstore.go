// Package jobstore keeps job events in Postgres, as the hosted service's record
// of what ran, on what, and how long each stage took.
//
// Record never waits. Events go through a bounded queue to one writer, which
// flushes about once a second, so a slow or missing database costs detail, never
// a job. The writer's upserts are safe to repeat and to apply in any order: a
// timestamp a later event sets is never cleared by an earlier one.
package jobstore

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/firerunnerhq/firerunner/internal/jobevent"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Tuning, as variables for tests.
var (
	queueSize     = 20000
	batchSize     = 500
	flushEvery    = time.Second
	retryWait     = 5 * time.Second
	monthsAhead   = 2
	partitionTick = 24 * time.Hour
)

// Store writes events to Postgres.
type Store struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	queue   chan jobevent.Event
	dropped atomic.Int64
	stop    chan struct{}
	done    chan struct{}
}

// Open connects, applies migrations and starts the writer. Close ends it.
func Open(ctx context.Context, dsn string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("job store: %w", err)
	}
	s := &Store{pool: pool, log: log, queue: make(chan jobevent.Event, queueSize), stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.ensurePartitions(ctx, time.Now()); err != nil {
		pool.Close()
		return nil, err
	}
	go s.run()
	return s, nil
}

// Record implements jobevent.Sink. It drops the event when the queue is full.
func (s *Store) Record(e jobevent.Event) {
	select {
	case s.queue <- e:
	default:
		s.dropped.Add(1)
	}
}

// Dropped counts events lost to a full queue.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// Close flushes what it can within ctx and stops the writer.
func (s *Store) Close(ctx context.Context) {
	close(s.stop)
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	s.pool.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("job store: migrations table: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.apply(ctx, name); err != nil {
			return fmt.Errorf("job store: %s: %w", name, err)
		}
	}
	return nil
}

// apply runs one migration under a lock, so two starting shards cannot race.
func (s *Store) apply(ctx context.Context, name string) error {
	sql, err := migrations.ReadFile(name)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7231905)`); err != nil {
			return err
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			return nil
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
		return err
	})
}

// ensurePartitions creates this month's event partition and those ahead.
func (s *Store) ensurePartitions(ctx context.Context, now time.Time) error {
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := range monthsAhead + 1 {
		from, to := month.AddDate(0, i, 0), month.AddDate(0, i+1, 0)
		sql := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS job_events_%s PARTITION OF job_events FOR VALUES FROM ('%s') TO ('%s')`,
			from.Format("2006_01"), from.Format(time.DateOnly), to.Format(time.DateOnly))
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("job store: partition: %w", err)
		}
	}
	return nil
}

func (s *Store) run() {
	defer close(s.done)
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	partitions := time.NewTicker(partitionTick)
	defer partitions.Stop()
	var batch []jobevent.Event
	for {
		select {
		case e := <-s.queue:
			batch = append(batch, e)
			if len(batch) >= batchSize {
				batch = s.flush(batch)
			}
		case <-flush.C:
			batch = s.flush(batch)
		case <-partitions.C:
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := s.ensurePartitions(ctx, time.Now()); err != nil {
				s.log.Warn("job store: could not add partitions", "err", err)
			}
			cancel()
		case <-s.stop:
			for {
				select {
				case e := <-s.queue:
					batch = append(batch, e)
					continue
				default:
				}
				break
			}
			s.flush(batch)
			return
		}
	}
}

// flush writes batch and returns what is left to try again: nothing on success,
// and the batch on failure, up to one queue's worth.
func (s *Store) flush(batch []jobevent.Event) []jobevent.Event {
	if len(batch) == 0 {
		return batch
	}
	ctx, cancel := context.WithTimeout(context.Background(), retryWait)
	defer cancel()
	if err := s.write(ctx, batch); err != nil {
		s.log.Warn("job store: write failed; keeping events to retry", "events", len(batch), "err", err)
		if len(batch) >= queueSize {
			s.dropped.Add(int64(len(batch)))
			return nil
		}
		return batch
	}
	return nil
}

func (s *Store) write(ctx context.Context, events []jobevent.Event) error {
	b := &pgx.Batch{}
	for _, e := range events {
		queue(b, e)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
}

const insertEvent = `INSERT INTO job_events (at, kind, tier, runner, job_id, repository, run_id, detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// A column set by any event stays set: COALESCE and NULLIF let events arrive in
// any order, and more than once.
const upsertJob = `INSERT INTO jobs (job_id, repository, workflow, job_name, event_name, run_id, tier, runner, queued_at, assigned_at, started_at, completed_at, result)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (job_id) DO UPDATE SET
    repository = COALESCE(NULLIF(EXCLUDED.repository, ''), jobs.repository),
    workflow = COALESCE(NULLIF(EXCLUDED.workflow, ''), jobs.workflow),
    job_name = COALESCE(NULLIF(EXCLUDED.job_name, ''), jobs.job_name),
    event_name = COALESCE(NULLIF(EXCLUDED.event_name, ''), jobs.event_name),
    run_id = GREATEST(EXCLUDED.run_id, jobs.run_id),
    tier = COALESCE(NULLIF(EXCLUDED.tier, ''), jobs.tier),
    runner = COALESCE(NULLIF(EXCLUDED.runner, ''), jobs.runner),
    queued_at = COALESCE(EXCLUDED.queued_at, jobs.queued_at),
    assigned_at = COALESCE(EXCLUDED.assigned_at, jobs.assigned_at),
    started_at = COALESCE(EXCLUDED.started_at, jobs.started_at),
    completed_at = COALESCE(EXCLUDED.completed_at, jobs.completed_at),
    result = COALESCE(NULLIF(EXCLUDED.result, ''), jobs.result)`

const upsertRunner = `INSERT INTO runners (runner, tier, instance, instance_type, market, subnet, zone, path, launched_at, served_at, listening_at, running_at, exited_at, exit_code, gone_at, gone_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (runner) DO UPDATE SET
    tier = COALESCE(NULLIF(EXCLUDED.tier, ''), runners.tier),
    instance = COALESCE(NULLIF(EXCLUDED.instance, ''), runners.instance),
    instance_type = COALESCE(NULLIF(EXCLUDED.instance_type, ''), runners.instance_type),
    market = COALESCE(NULLIF(EXCLUDED.market, ''), runners.market),
    subnet = COALESCE(NULLIF(EXCLUDED.subnet, ''), runners.subnet),
    zone = COALESCE(NULLIF(EXCLUDED.zone, ''), runners.zone),
    path = COALESCE(NULLIF(EXCLUDED.path, ''), runners.path),
    launched_at = COALESCE(EXCLUDED.launched_at, runners.launched_at),
    served_at = COALESCE(EXCLUDED.served_at, runners.served_at),
    listening_at = COALESCE(EXCLUDED.listening_at, runners.listening_at),
    running_at = COALESCE(EXCLUDED.running_at, runners.running_at),
    exited_at = COALESCE(EXCLUDED.exited_at, runners.exited_at),
    exit_code = COALESCE(EXCLUDED.exit_code, runners.exit_code),
    gone_at = COALESCE(EXCLUDED.gone_at, runners.gone_at),
    gone_reason = COALESCE(NULLIF(EXCLUDED.gone_reason, ''), runners.gone_reason)`

func queue(b *pgx.Batch, e jobevent.Event) {
	detail, _ := json.Marshal(e)
	b.Queue(insertEvent, e.At, string(e.Kind), e.Tier, e.Runner, e.JobID, e.Repository, e.RunID, detail)
	if e.JobID != "" {
		b.Queue(upsertJob, e.JobID, e.Repository, e.Workflow, e.JobName, e.EventName, e.RunID, e.Tier, e.Runner,
			when(e.QueuedAt, !e.QueuedAt.IsZero()), when(e.At, e.Kind == jobevent.Assigned),
			when(e.At, e.Kind == jobevent.Started), when(e.At, e.Kind == jobevent.Completed), e.Result)
	}
	if e.JobID == "" && e.Runner != "" {
		var code *int32
		if e.Kind == jobevent.Exited {
			c := int32(e.ExitCode)
			code = &c
		}
		b.Queue(upsertRunner, e.Runner, e.Tier, e.Instance, e.InstanceType, e.Market, e.Subnet, e.Zone, e.Path,
			when(e.At, e.Kind == jobevent.Launched), when(e.At, e.Kind == jobevent.Served),
			when(e.At, e.Kind == jobevent.Listening), when(e.At, e.Kind == jobevent.Running),
			when(e.At, e.Kind == jobevent.Exited), code, when(e.At, e.Kind == jobevent.InstanceGone), e.Reason)
	}
}

// when is t if ok, else NULL.
func when(t time.Time, ok bool) *time.Time {
	if !ok {
		return nil
	}
	return &t
}
