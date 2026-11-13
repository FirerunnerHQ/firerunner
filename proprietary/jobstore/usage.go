package jobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageSchema is the version of the usage event's payload. Adding a field
// keeps it; changing or removing one makes a new version.
const UsageSchema = 1

// Usage is one usage event's payload, as usage-event.schema.json describes
// it: a job a runner ran, which tenant it belongs to, what ran it, and for
// how long. Billing rates it; the store never knows a price.
type Usage struct {
	Schema  int    `json:"schema"`
	EventID string `json:"event_id"`
	Kind    string `json:"kind"`
	Tenant  string `json:"tenant"`

	Repository string `json:"repository"`
	Workflow   string `json:"workflow"`
	JobName    string `json:"job_name"`
	JobID      string `json:"job_id"`
	RunID      int64  `json:"run_id"`
	EventName  string `json:"event_name"`
	Result     string `json:"result"`

	Tier         string `json:"tier"`
	Runner       string `json:"runner"`
	InstanceType string `json:"instance_type"`
	Market       string `json:"market"`
	Zone         string `json:"zone"`
	Supply       string `json:"supply"`

	// StartedAt is GitHub's start of the job, or else the runner's
	// report of taking it (StartSource says which); nil when neither
	// arrived, and then DurationMS is 0.
	StartedAt   *time.Time `json:"started_at"`
	StartSource string     `json:"start_source"`
	CompletedAt time.Time  `json:"completed_at"`
	DurationMS  int64      `json:"duration_ms"`
}

// Usage timing, as variables for tests.
var (
	// usageEvery is how often the writer looks for jobs to write usage for.
	usageEvery = 15 * time.Second
	// usageGrace is how long a completed job whose start is not known yet
	// waits for one (its runner reports taking it once the runner exits,
	// after GitHub reports the end) before its usage is written without.
	usageGrace = 10 * time.Minute
	// usageWindow is how far back the sweep looks: a job completed longer
	// ago than this without usage stays without.
	usageWindow = 7 * 24 * time.Hour
)

// sweepUsage writes the usage event of each completed job a runner ran that
// has none, once the job is settled: its start known, from GitHub or its
// runner, or its grace ($2) passed since its end. It reads the rows as they
// stand, so the order events arrived in, and the batch each came in, do
// not matter. event_id is the job's, so a job is written once, however
// often its events arrive or are replayed, and however many shards sweep.
// $1 is now.
const sweepUsage = `INSERT INTO usage_events (event_id, tenant, job_id, payload)
SELECT 'job/' || j.job_id, j.tenant, j.job_id, jsonb_build_object(
    'schema', 1,
    'event_id', 'job/' || j.job_id,
    'kind', 'job',
    'tenant', j.tenant,
    'repository', j.repository,
    'workflow', j.workflow,
    'job_name', j.job_name,
    'job_id', j.job_id,
    'run_id', j.run_id,
    'event_name', j.event_name,
    'result', j.result,
    'tier', j.tier,
    'runner', j.runner,
    'instance_type', COALESCE(r.instance_type, ''),
    'market', COALESCE(r.market, ''),
    'zone', COALESCE(r.zone, ''),
    'supply', COALESCE(r.path, ''),
    'started_at', to_char(s.at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'start_source', s.source,
    'completed_at', to_char(j.completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'duration_ms', COALESCE(GREATEST(0, floor(EXTRACT(EPOCH FROM j.completed_at - s.at) * 1000))::bigint, 0))
FROM jobs j
LEFT JOIN runners r ON r.tenant = j.tenant AND r.runner = j.runner
CROSS JOIN LATERAL (SELECT COALESCE(j.started_at, r.running_at) AS at,
    CASE WHEN j.started_at IS NOT NULL THEN 'github' WHEN r.running_at IS NOT NULL THEN 'runner' ELSE 'none' END AS source) s
WHERE j.completed_at IS NOT NULL AND j.runner <> ''
    AND j.completed_at > $1::timestamptz - $3::interval
    AND (s.source <> 'none' OR j.completed_at < $1::timestamptz - $2::interval)
    AND NOT EXISTS (SELECT 1 FROM usage_events u WHERE u.event_id = 'job/' || j.job_id)
ORDER BY j.completed_at, j.job_id
ON CONFLICT (event_id) DO NOTHING`

// sweep writes the usage of the jobs settled by now.
func (s *Store) sweep(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, sweepUsage, now, usageGrace, usageWindow)
	return err
}

// UsageEntry is one usage event as read, with the cursor that reads on
// from it.
type UsageEntry struct {
	Cursor string          `json:"cursor"`
	Event  json.RawMessage `json:"event"`
}

// Cursor is a position in the usage events: the writing transaction's id
// and the row's sequence number. The zero Cursor is the start.
type Cursor struct {
	Xact uint64
	Seq  int64
}

func (c Cursor) String() string { return fmt.Sprintf("%d.%d", c.Xact, c.Seq) }

// ParseCursor reads a cursor ReadUsage returned; "" is the start.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	x, q, ok := strings.Cut(s, ".")
	xact, err1 := strconv.ParseUint(x, 10, 64)
	seq, err2 := strconv.ParseInt(q, 10, 64)
	if !ok || err1 != nil || err2 != nil || seq < 0 {
		return Cursor{}, fmt.Errorf("cursor %q: give one a read returned, or none for the start", s)
	}
	return Cursor{xact, seq}, nil
}

// readUsage pages in (xact, seq) order and stops below the oldest
// transaction still open: identity values are taken in insert order but
// committed in commit order, so a seq cursor alone would skip a row whose
// transaction commits after a later one's. Every transaction that may yet
// commit a row has an id at or above that bound, so a row never appears
// behind a cursor already read past it.
const readUsage = `SELECT xact::text, seq, payload FROM usage_events
WHERE (xact, seq) > ($1::text::xid8, $2::bigint) AND xact < pg_snapshot_xmin(pg_current_snapshot())
ORDER BY xact, seq
LIMIT $3`

// UsageReader reads the usage events a store wrote.
type UsageReader struct {
	pool *pgxpool.Pool
}

// OpenUsage connects to the store's database. It fails only on a URL that
// can never work, without echoing the URL; a database that is down fails
// the read.
func OpenUsage(dsn string) (*UsageReader, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("job store: the database URL is not valid")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, errors.New("job store: the database URL is not valid")
	}
	return &UsageReader{pool: pool}, nil
}

// Close closes the connections.
func (r *UsageReader) Close() { r.pool.Close() }

// Read returns up to limit events after cursor, oldest first. Fewer than
// limit means no more can be read yet: none committed after them, or a
// transaction older than them, anywhere on the database's server, is still
// open (an idle one holds the feed back until it ends; a role's
// idle_in_transaction_session_timeout bounds that). Keep polling. An event
// can be read again (after a consumer loses its cursor, say): consumers
// dedupe on event_id. A cursor past every transaction this database has
// started fails: it came from another database, or from this one before a
// restore or a move to a new server, and reading on from it would skip
// every new event.
func (r *UsageReader) Read(ctx context.Context, after Cursor, limit int) ([]UsageEntry, error) {
	return readUsageFrom(ctx, r.pool, after, limit)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrCursorAhead is a cursor from another database, or from before a
// restore: start again from the beginning, deduping on event_id.
var ErrCursorAhead = errors.New("job store: the cursor is past every transaction this database has started: it is from another database, or from before a restore or a move to a new server; read again from the start (--after none given) and count each event_id once")

func readUsageFrom(ctx context.Context, q querier, after Cursor, limit int) ([]UsageEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	if after.Xact > 0 {
		var ahead bool
		err := q.QueryRow(ctx, `SELECT $1::text::xid8 >= pg_snapshot_xmax(pg_current_snapshot())`, strconv.FormatUint(after.Xact, 10)).Scan(&ahead)
		if err != nil {
			return nil, usageErr(err)
		}
		if ahead {
			return nil, ErrCursorAhead
		}
	}
	rows, err := q.Query(ctx, readUsage, strconv.FormatUint(after.Xact, 10), after.Seq, limit)
	if err != nil {
		return nil, usageErr(err)
	}
	var out []UsageEntry
	for rows.Next() {
		var c Cursor
		var payload []byte
		var xact string
		if err := rows.Scan(&xact, &c.Seq, &payload); err != nil {
			rows.Close()
			return nil, usageErr(err)
		}
		if c.Xact, err = strconv.ParseUint(xact, 10, 64); err != nil {
			rows.Close()
			return nil, fmt.Errorf("job store: transaction id %q: %w", xact, err)
		}
		out = append(out, UsageEntry{Cursor: c.String(), Event: payload})
	}
	if err := rows.Err(); err != nil {
		return nil, usageErr(err)
	}
	return out, nil
}

// usageErr says what failed without the server's own words, which can name
// a host or user; a missing table means no daemon has set the store up yet.
func usageErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "42P01" {
		return errors.New("job store: no usage events table: a daemon with FR_JOB_EVENTS_DSN sets it up when it starts")
	}
	return errors.New("job store: " + describe(err))
}
