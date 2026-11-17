package capacity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// CallSource reads a window's workload calls by UTC day.
type CallSource interface {
	Read(ctx context.Context, from, to time.Time) ([]DayCalls, error)
}

// readTimeout bounds each statement of the read (the history store may
// be the monitored database).
const readTimeout = "30s"

// dayCallsSQL is each UTC day of [$1, $2): its calls of every statement
// but pg_sage's own, and how much the rollup saw of it (rollup rows plus
// coverage hours). A (queryid, day) comes from the daily tier when it has
// a row, else from the hourly tier. A statement's text is its latest in
// each day's last queries snapshot, as the evidence pack reads it; with
// no text it counts as workload (selfmonitor.StatementExclusionSQL).
var dayCallsSQL = `/* pg_sage review:capacity */ WITH
d AS (SELECT r.queryid, r.day, r.calls FROM sage.query_rollup_daily r
       WHERE {db:r} AND r.day >= ($1::timestamptz AT TIME ZONE 'UTC')::date
         AND r.day < ($2::timestamptz AT TIME ZONE 'UTC')::date),
h AS (SELECT r.queryid, (r.hour AT TIME ZONE 'UTC')::date AS day, sum(r.calls)::int8 AS calls
        FROM sage.query_rollup_hourly r
       WHERE {db:r} AND r.hour >= $1 AND r.hour < $2 GROUP BY 1, 2),
c AS (SELECT d.queryid, d.day, d.calls FROM d
      UNION ALL
      SELECT h.queryid, h.day, h.calls FROM h
       WHERE NOT EXISTS (SELECT 1 FROM d WHERE d.queryid = h.queryid AND d.day = h.day)),
p AS (SELECT DISTINCT ON ((s.collected_at AT TIME ZONE 'UTC')::date) s.id
        FROM sage.snapshots s
       WHERE {db:s} AND s.category = 'queries' AND s.collected_at >= $1
         AND s.collected_at < $2 AND ` + snapstore.NonEmptySQL("s") + `
       ORDER BY (s.collected_at AT TIME ZONE 'UTC')::date, s.collected_at DESC, s.id DESC),
t AS (SELECT DISTINCT ON (e.queryid) e.queryid, e.query FROM (
        SELECT (x->>'queryid')::int8 AS queryid, x->>'query' AS query, q.collected_at, q.id
          FROM sage.snapshots q
         CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("q") + `) x
         WHERE {db:q} AND q.id IN (SELECT p.id FROM p)) e
       WHERE e.query IS NOT NULL
       ORDER BY e.queryid, e.collected_at DESC, e.id DESC),
k AS (SELECT c.day,
             COALESCE(sum(c.calls) FILTER (WHERE ` +
	selfmonitor.StatementExclusionSQL("t.query") + `), 0)::int8 AS workload,
             count(*)::int AS n
        FROM c LEFT JOIN t ON t.queryid = c.queryid GROUP BY c.day),
v AS (SELECT (g.hour AT TIME ZONE 'UTC')::date AS day, count(*)::int AS hours
        FROM sage.query_rollup_coverage g
       WHERE {db:g} AND g.hour >= $1 AND g.hour < $2 GROUP BY 1)
SELECT COALESCE(k.day, v.day), COALESCE(k.workload, 0)::int8,
       (COALESCE(k.n, 0) + COALESCE(v.hours, 0))::int
  FROM k FULL JOIN v ON v.day = k.day ORDER BY 1`

// errNoHistory is a source with no history store.
var errNoHistory = errors.New("capacity source: no history store")

// DBCallSource reads pg_sage's history store (histstore.Resolve of db):
// the query rollups (evid-01) and the queries snapshots' texts, in one
// read-only transaction bounded by readTimeout. It writes nothing.
type DBCallSource struct{ db any }

// NewDBCallSource is a source over db's history store.
func NewDBCallSource(db any) *DBCallSource { return &DBCallSource{db: db} }

var _ CallSource = (*DBCallSource)(nil)

// Read is every UTC day of [from, to), oldest first; a day with no
// history is unobserved.
func (s *DBCallSource) Read(ctx context.Context, from, to time.Time) ([]DayCalls, error) {
	if s == nil || s.db == nil {
		return nil, errNoHistory
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("capacity source: empty window [%s, %s)",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	seen, err := s.readDays(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the workload calls by day from sage.query_rollup_daily, "+
			"sage.query_rollup_hourly, sage.query_rollup_coverage and sage.snapshots "+
			"(queries) for [%s, %s): %w", from.Format(time.RFC3339), to.Format(time.RFC3339),
			err)
	}
	var out []DayCalls
	for d := dayStart(from); d.Before(to); d = d.AddDate(0, 0, 1) {
		day := seen[d.Format(time.DateOnly)]
		day.Day = d
		out = append(out, day)
	}
	return out, nil
}

// readDays runs dayCallsSQL in a bounded read-only transaction.
func (s *DBCallSource) readDays(ctx context.Context, from, to time.Time,
) (map[string]DayCalls, error) {
	st := histstore.Resolve(s.db)
	tx, err := st.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, stmt := range []string{"SET TRANSACTION READ ONLY",
		"SET LOCAL statement_timeout = '" + readTimeout + "'"} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	rows, err := st.WithDB(tx).Query(ctx, dayCallsSQL, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]DayCalls{}
	for rows.Next() {
		var day time.Time
		var calls int64
		var seenCount int
		if err := rows.Scan(&day, &calls, &seenCount); err != nil {
			return nil, err
		}
		seen[day.Format(time.DateOnly)] = DayCalls{Observed: seenCount > 0,
			WorkloadCalls: calls}
	}
	return seen, rows.Err()
}
