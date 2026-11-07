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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	writeTimeout  = 5 * time.Second
	retryWait     = 5 * time.Second
	monthsAhead   = 2
	partitionTick = 24 * time.Hour
)

// Store writes events to Postgres.
type Store struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	queue    chan jobevent.Event
	dropped  atomic.Int64
	reported atomic.Int64
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	// lossEvery is how often lost events are reported.
	lossEvery time.Duration
	// write and prepare are the database's side, replaced in tests.
	write   func(context.Context, []jobevent.Event) error
	prepare func(context.Context) error
}

// New starts the writer and returns at once: the database may be down, so the
// writer connects and migrates in its own time, and events wait in the queue. It
// fails only on a URL that can never work, without echoing the URL.
func New(dsn string, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("job store: the database URL is not valid")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, errors.New("job store: the database URL is not valid")
	}
	s := newStore(log)
	s.pool, s.write, s.prepare = pool, s.writeEvents, s.setUp
	go s.run()
	return s, nil
}

func newStore(log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{log: log, lossEvery: time.Minute, queue: make(chan jobevent.Event, queueSize), stop: make(chan struct{}), done: make(chan struct{})}
}

// Record implements jobevent.Sink. It drops the event when the queue is full.
func (s *Store) Record(e jobevent.Event) {
	select {
	case s.queue <- e:
	default:
		s.dropped.Add(1)
	}
}

// Dropped counts events lost to a full queue, a rejected row or a failed write at
// close. A close that gives up on a hung writer cannot count what the writer still
// holds, so after one this is a lower bound.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// Close flushes what it can within ctx and stops the writer.
func (s *Store) Close(ctx context.Context) {
	s.once.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	s.reportLoss() // the writer may still be stuck, and the process exits next
	if s.pool != nil {
		s.pool.Close()
	}
}

// setUp migrates and creates partitions under one lock, so shards starting
// together on an empty database wait for each other.
func (s *Store) setUp(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7231905)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		names, err := fs.Glob(migrations, "migrations/*.sql")
		if err != nil {
			return err
		}
		sort.Strings(names)
		for _, name := range names {
			if err := applyMigration(ctx, tx, name); err != nil {
				return err
			}
		}
		return ensurePartitions(ctx, tx, time.Now())
	})
}

func applyMigration(ctx context.Context, tx pgx.Tx, name string) error {
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	sql, err := migrations.ReadFile(name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
	return err
}

// ensurePartitions creates this month's event partition and those ahead, with
// bounds in UTC whatever the session's time zone. An event outside them falls in
// the default partition.
func ensurePartitions(ctx context.Context, tx pgx.Tx, now time.Time) error {
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := range monthsAhead + 1 {
		if err := ensurePartition(ctx, tx, month.AddDate(0, i, 0)); err != nil {
			return err
		}
	}
	return nil
}

// ensurePartition creates one month's partition. Postgres refuses while the
// default partition holds rows of that month, so they move across.
func ensurePartition(ctx context.Context, tx pgx.Tx, from time.Time) error {
	name := "job_events_" + from.Format("2006_01")
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	lo, hi := from.Format(time.DateOnly)+" 00:00:00+00", from.AddDate(0, 1, 0).Format(time.DateOnly)+" 00:00:00+00"
	for _, sql := range []string{
		fmt.Sprintf(`CREATE TEMP TABLE moved ON COMMIT DROP AS WITH d AS (DELETE FROM job_events_default WHERE at >= '%s' AND at < '%s' RETURNING *) SELECT * FROM d`, lo, hi),
		fmt.Sprintf(`CREATE TABLE %s PARTITION OF job_events FOR VALUES FROM ('%s') TO ('%s')`, name, lo, hi),
		`INSERT INTO job_events SELECT * FROM moved`,
		`DROP TABLE moved`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return nil
}

// run owns the database's side. Until set-up succeeds, events wait in the queue.
func (s *Store) run() {
	defer close(s.done)
	defer s.reportLoss() // before done, so a closing daemon logs it
	go s.reportLosses()
	if !s.retry(s.prepare, "set up") {
		s.dropQueued()
		return
	}
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
				s.flush(batch)
				batch = nil
			}
		case <-flush.C:
			s.flush(batch)
			batch = nil
		case <-partitions.C:
			s.attempt(s.prepare, "add partitions")
		case <-s.stop:
			s.drain(batch)
			return
		}
	}
}

// drain makes one last try at everything left, and counts what it loses.
func (s *Store) drain(batch []jobevent.Event) {
	for {
		select {
		case e := <-s.queue:
			batch = append(batch, e)
			continue
		default:
		}
		break
	}
	for len(batch) > 0 {
		n := min(batchSize, len(batch))
		s.flush(batch[:n])
		batch = batch[n:]
	}
}

// reportLosses logs lost events on its own clock, as the writer may be busy
// waiting for the database.
func (s *Store) reportLosses() {
	tick := time.NewTicker(s.lossEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			s.reportLoss()
		case <-s.done:
			return
		}
	}
}

// reportLoss logs the total when it has grown since the last report.
func (s *Store) reportLoss() {
	n := s.dropped.Load()
	if s.reported.Swap(n) != n {
		s.log.Warn("job store: events lost", "total", n)
	}
}

// dropQueued counts what is queued when the store never reached the database.
func (s *Store) dropQueued() {
	for {
		select {
		case <-s.queue:
			s.dropped.Add(1)
		default:
			return
		}
	}
}

// flush writes batch. A row the database rejects is isolated and dropped, so one
// bad event cannot stop every other. Any other failure waits and tries again, and
// meanwhile the queue fills, and then drops what is newest.
func (s *Store) flush(batch []jobevent.Event) {
	if len(batch) == 0 {
		return
	}
	var err error
	done := s.retry(func(ctx context.Context) error {
		err = s.write(ctx, batch)
		if rejected(err) {
			return nil
		}
		return err
	}, "write")
	if !done {
		s.dropped.Add(int64(len(batch)))
		return
	}
	if !rejected(err) {
		return
	}
	if len(batch) == 1 {
		s.dropped.Add(1)
		s.log.Warn("job store: dropped an event the database rejected", "kind", batch[0].Kind, "err", describe(err))
		return
	}
	mid := len(batch) / 2
	s.flush(batch[:mid])
	s.flush(batch[mid:])
}

// rejected reports a row the database cannot take, which no retry can change:
// data (22) and constraint (23) errors. Anything else, such as a rotated password,
// a failover or a full disk, is about the database, and every row must wait it out.
func rejected(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (strings.HasPrefix(pg.Code, "22") || strings.HasPrefix(pg.Code, "23"))
}

// retry calls fn until it succeeds or the store stops, and reports success.
func (s *Store) retry(fn func(context.Context) error, what string) bool {
	for {
		if s.attempt(fn, what) {
			return true
		}
		select {
		case <-s.stop:
			return s.attempt(fn, what)
		case <-time.After(retryWait):
		}
	}
}

func (s *Store) attempt(fn func(context.Context) error, what string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		s.log.Warn("job store: "+what+" failed", "err", describe(err))
		return false
	}
	return true
}

// describe says what failed without a host, user or database, which a server's
// messages can name too.
func describe(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return "SQLSTATE " + pg.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return "could not reach the database"
}

func (s *Store) writeEvents(ctx context.Context, events []jobevent.Event) error {
	b := &pgx.Batch{}
	for _, e := range events {
		queue(b, clean(e))
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
}

// clean removes NUL, which Postgres text cannot hold. Job names come from users.
func clean(e jobevent.Event) jobevent.Event {
	strip := func(v string) string { return strings.ReplaceAll(v, "\x00", "") }
	e.Tier, e.Runner, e.JobID, e.Repository = strip(e.Tier), strip(e.Runner), strip(e.JobID), strip(e.Repository)
	e.Workflow, e.JobName, e.EventName, e.Result, e.Reason = strip(e.Workflow), strip(e.JobName), strip(e.EventName), strip(e.Result), strip(e.Reason)
	labels := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		labels[i] = strip(l)
	}
	e.Labels = labels
	return e
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
