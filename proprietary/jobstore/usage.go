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

// insertUsage writes the usage event of a completed job a runner ran, from
// its rows as they stand, once: event_id is the job's, so a redelivered
// completion, or a replay of the events, adds nothing.
const insertUsage = `INSERT INTO usage_events (event_id, tenant, job_id, payload)
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
WHERE j.job_id = $1 AND j.completed_at IS NOT NULL AND j.runner <> ''
ON CONFLICT (event_id) DO NOTHING`

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
// limit means none committed after them yet. An event can be read again
// (after a consumer loses its cursor, say): consumers dedupe on event_id.
func (r *UsageReader) Read(ctx context.Context, after Cursor, limit int) ([]UsageEntry, error) {
	return readUsageFrom(ctx, r.pool, after, limit)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readUsageFrom(ctx context.Context, q querier, after Cursor, limit int) ([]UsageEntry, error) {
	if limit <= 0 {
		return nil, nil
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
