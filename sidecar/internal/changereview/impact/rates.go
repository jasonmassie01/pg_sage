package impact

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// writeRate is a table's inserts, updates and deletes per second: the
// live counters against the newest 'tables' snapshot at least an hour
// old. known is false without such a snapshot or after a counter reset.
type writeRate struct {
	known         bool
	ins, upd, del float64
	since         time.Time
}

// writeRateAge is how old the snapshot a write rate starts from must be.
const writeRateAge = time.Hour

var writeSnapshotSQL = `/* pg_sage change_review_write_snapshot v1 */
	WITH s AS (
	    SELECT s.collected_at, ` + snapstore.DataSQL("s") + ` AS doc
	      FROM sage.snapshots s
	     WHERE {db:s} AND s.category = 'tables' AND s.collected_at <= $1
	     ORDER BY s.collected_at DESC LIMIT 1)
	SELECT s.collected_at, e->>'schemaname', e->>'relname',
	       coalesce((e->>'n_tup_ins')::int8, 0), coalesce((e->>'n_tup_upd')::int8, 0),
	       coalesce((e->>'n_tup_del')::int8, 0)
	  FROM s CROSS JOIN LATERAL jsonb_array_elements(
	         CASE WHEN jsonb_typeof(s.doc) = 'array' THEN s.doc ELSE '[]'::jsonb END) e
	 WHERE (e->>'schemaname', e->>'relname') IN (SELECT * FROM unnest($2::text[], $3::text[]))`

type counters struct{ ins, upd, del int64 }

// loadWriteRates fills the write rate of every relation found.
func (r *run) loadWriteRates() {
	var oids []uint32
	byOID := map[uint32]*relation{}
	var schemas, names []string
	for _, rel := range r.rels {
		if rel.found {
			oids = append(oids, rel.oid)
			byOID[rel.oid] = rel
			schemas, names = append(schemas, rel.schema), append(names, rel.rel)
		}
	}
	if len(oids) == 0 {
		return
	}
	live, err := r.liveCounters(oids)
	if r.readFailed(err, nil, "live write counters") {
		return
	}
	at, snap, err := r.snapshotCounters(schemas, names)
	if r.readFailed(err, nil, "tables snapshot") || at.IsZero() {
		return
	}
	elapsed := r.e.d.Now().Sub(at).Seconds()
	for oid, rel := range byOID {
		cur, okLive := live[oid]
		old, okSnap := snap[rel.name]
		if !okLive || !okSnap || elapsed <= 0 || cur.ins < old.ins || cur.upd < old.upd ||
			cur.del < old.del {
			continue
		}
		rel.rate = writeRate{known: true, since: at,
			ins: float64(cur.ins-old.ins) / elapsed, upd: float64(cur.upd-old.upd) / elapsed,
			del: float64(cur.del-old.del) / elapsed}
	}
}

func (r *run) liveCounters(oids []uint32) (map[uint32]counters, error) {
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_write_live v1 */
		SELECT relid, n_tup_ins, n_tup_upd, n_tup_del FROM pg_catalog.pg_stat_all_tables
		WHERE relid = ANY($1::oid[])`, oids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]counters{}
	for rows.Next() {
		var oid uint32
		var c counters
		if err := rows.Scan(&oid, &c.ins, &c.upd, &c.del); err != nil {
			return nil, err
		}
		out[oid] = c
	}
	return out, rows.Err()
}

func (r *run) snapshotCounters(schemas, names []string) (time.Time,
	map[string]counters, error) {
	ctx, cancel := r.historyContext()
	defer cancel()
	rows, err := histstore.Resolve(r.e.d.Pool).Query(ctx, writeSnapshotSQL,
		r.e.d.Now().Add(-writeRateAge), schemas, names)
	if err != nil {
		return time.Time{}, nil, r.historyErr(ctx, err)
	}
	defer rows.Close()
	var at time.Time
	out := map[string]counters{}
	for rows.Next() {
		var schema, name string
		var c counters
		if err := rows.Scan(&at, &schema, &name, &c.ins, &c.upd, &c.del); err != nil {
			return time.Time{}, nil, err
		}
		out[schema+"."+name] = c
	}
	return at, out, r.historyErr(ctx, rows.Err())
}

// historyContext bounds one history read (sage.snapshots, query_store),
// which runs through histstore rather than a bounded catalog read.
func (r *run) historyContext() (context.Context, context.CancelFunc) {
	if t := r.e.d.Timeouts.Statement; t > 0 {
		return context.WithTimeout(r.ctx, t)
	}
	return context.WithCancel(r.ctx)
}

// historyErr reports a history read cut off by its own bound as a timed
// out read (a *catalogread.PhaseError), so it becomes CR-17 evidence.
func (r *run) historyErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if r.ctx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &catalogread.PhaseError{Phase: catalogread.PhaseExecution,
			Err: fmt.Errorf("history read: %w", err)}
	}
	return err
}

// queryRatesSQL is each queryid's calls over the window and its peak
// calls per second. It reads at most one sample per queryid and hour
// (the first of the hour, by index) plus the newest one, never every
// sample of a mature history: the rate between consecutive such anchors
// of one statistics epoch is an hourly rate, and the busiest is the peak.
const queryRatesSQL = `/* pg_sage change_review_query_rates v2 */
	WITH ids AS (SELECT DISTINCT unnest($1::int8[]) AS queryid),
	hours AS (SELECT generate_series(date_trunc('hour', $2::timestamptz), $3::timestamptz,
	                                 interval '1 hour') AS h),
	a AS (
	    SELECT i.queryid, s.captured_at, s.calls, s.stats_epoch
	      FROM ids i CROSS JOIN hours h
	      CROSS JOIN LATERAL (
	        SELECT q.captured_at, q.calls, q.stats_epoch FROM sage.query_store q
	         WHERE {db:q} AND q.queryid = i.queryid AND q.captured_at >= greatest(h.h, $2)
	           AND q.captured_at < h.h + interval '1 hour'
	         ORDER BY q.captured_at LIMIT 1) s
	    UNION
	    SELECT i.queryid, s.captured_at, s.calls, s.stats_epoch
	      FROM ids i CROSS JOIN LATERAL (
	        SELECT q.captured_at, q.calls, q.stats_epoch FROM sage.query_store q
	         WHERE {db:q} AND q.queryid = i.queryid AND q.captured_at >= $2
	         ORDER BY q.captured_at DESC LIMIT 1) s
	), d AS (
	    SELECT queryid, calls - lag(calls) OVER w AS dcalls,
	           extract(epoch FROM captured_at - lag(captured_at) OVER w)::float8 AS dt,
	           stats_epoch IS NOT DISTINCT FROM lag(stats_epoch) OVER w AS same_epoch
	      FROM a WINDOW w AS (PARTITION BY queryid ORDER BY captured_at)
	)
	SELECT queryid, coalesce(sum(dcalls), 0)::int8, coalesce(max(dcalls / dt), 0)::float8
	  FROM d WHERE same_epoch AND dcalls >= 0 AND dt > 0 GROUP BY queryid`

// rateWindow is the history the workload's calls and peak rate cover.
const rateWindow = 7 * 24 * time.Hour

// queryRate is one queryid's history.
type queryRate struct {
	calls int64
	peak  float64
}

func (r *run) queryRates(ids []int64) (map[int64]queryRate, error) {
	ctx, cancel := r.historyContext()
	defer cancel()
	now := r.e.d.Now()
	rows, err := histstore.Resolve(r.e.d.Pool).Query(ctx, queryRatesSQL, ids,
		now.Add(-rateWindow), now)
	if err != nil {
		return nil, r.historyErr(ctx, err)
	}
	defer rows.Close()
	out := map[int64]queryRate{}
	for rows.Next() {
		var id int64
		var q queryRate
		if err := rows.Scan(&id, &q.calls, &q.peak); err != nil {
			return nil, err
		}
		out[id] = q
	}
	return out, r.historyErr(ctx, rows.Err())
}
