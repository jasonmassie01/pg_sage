package querystore

import "github.com/pg-sage/sidecar/internal/snapstore"

// The rollup's statements (evid-01a). Each runs through a history Store
// (histstore markers) and is written once, here.

// counterBreak is a pair of expanded snapshot rows that cannot be
// differenced: R10's epoch break (EpochBreak), or a secondary counter that
// decreased (an evicted entry that came back) so no column can go negative.
func counterBreak(cur, prev string) string {
	out := "(" + EpochBreak(cur, prev)
	for _, col := range []string{"rows", "shared_blks_hit", "shared_blks_read",
		"temp_blks_read", "temp_blks_written", "wal_bytes"} {
		out += " OR " + cur + col + " < " + prev + col
	}
	return out + ")"
}

// systemEpochSQL is the statements epoch of the system snapshot of alias's
// collection time (the collector writes both in one transaction).
func systemEpochSQL(alias string) string {
	return `(SELECT (` + snapstore.DataSQL("y") + `->>'statements_epoch')::timestamptz
		FROM sage.snapshots y WHERE {db:y} AND y.category = 'system'
		  AND y.collected_at = ` + alias + `.collected_at LIMIT 1)`
}

// snapshotIntervalsSQL lists the pairs of consecutive queries snapshots of
// each queryid whose later member is in [$1, $2), at most $3 apart
// (AnchorLookback), for the queryids of $4 (all when empty), with their
// deltas, zero and flagged across a break. The expansion reads the
// snapshots from $1 - $3, so a pair's earlier member may be before $1.
var snapshotIntervalsSQL = `/* pg_sage */ WITH q AS (
	SELECT s.collected_at, ` + systemEpochSQL("s") + ` AS stats_epoch,
		r.queryid, r.calls, r.total_exec_time,
		COALESCE(r.rows, 0) AS rows, COALESCE(r.shared_blks_hit, 0) AS shared_blks_hit,
		COALESCE(r.shared_blks_read, 0) AS shared_blks_read,
		COALESCE(r.temp_blks_read, 0) AS temp_blks_read,
		COALESCE(r.temp_blks_written, 0) AS temp_blks_written,
		COALESCE(r.wal_bytes, 0) AS wal_bytes
	FROM sage.snapshots s
	CROSS JOIN LATERAL (SELECT ` + snapstore.DataSQL("s") + ` AS doc) d
	CROSS JOIN LATERAL jsonb_to_recordset(CASE WHEN jsonb_typeof(d.doc) = 'array'
		THEN d.doc ELSE '[]'::jsonb END) AS r(queryid int8, calls int8,
		total_exec_time float8, rows int8, shared_blks_hit int8, shared_blks_read int8,
		temp_blks_read int8, temp_blks_written int8, wal_bytes int8)
	WHERE {db:s} AND s.category = 'queries'
		AND s.collected_at >= $1::timestamptz - $3::interval AND s.collected_at < $2
		AND r.queryid IS NOT NULL AND r.queryid <> 0 AND r.calls IS NOT NULL
		AND r.total_exec_time IS NOT NULL
		AND (cardinality($4::int8[]) = 0 OR r.queryid = ANY($4::int8[]))
), p AS (
	SELECT q.*, lag(q.collected_at) OVER w AS prev_at,
		lag(q.stats_epoch) OVER w AS prev_stats_epoch, lag(q.calls) OVER w AS prev_calls,
		lag(q.total_exec_time) OVER w AS prev_total_exec_time,
		lag(q.rows) OVER w AS prev_rows, lag(q.shared_blks_hit) OVER w AS prev_shared_blks_hit,
		lag(q.shared_blks_read) OVER w AS prev_shared_blks_read,
		lag(q.temp_blks_read) OVER w AS prev_temp_blks_read,
		lag(q.temp_blks_written) OVER w AS prev_temp_blks_written,
		lag(q.wal_bytes) OVER w AS prev_wal_bytes
	FROM q WINDOW w AS (PARTITION BY q.queryid ORDER BY q.collected_at)
)
SELECT p.queryid, p.prev_at, p.collected_at, b.brk,
	CASE WHEN b.brk THEN 0 ELSE p.calls - p.prev_calls END,
	CASE WHEN b.brk THEN 0 ELSE p.total_exec_time - p.prev_total_exec_time END,
	CASE WHEN b.brk THEN 0 ELSE p.rows - p.prev_rows END,
	CASE WHEN b.brk THEN 0 ELSE p.shared_blks_hit - p.prev_shared_blks_hit END,
	CASE WHEN b.brk THEN 0 ELSE p.shared_blks_read - p.prev_shared_blks_read END,
	CASE WHEN b.brk THEN 0 ELSE p.temp_blks_read - p.prev_temp_blks_read END,
	CASE WHEN b.brk THEN 0 ELSE p.temp_blks_written - p.prev_temp_blks_written END,
	CASE WHEN b.brk THEN 0 ELSE p.wal_bytes - p.prev_wal_bytes END
FROM p CROSS JOIN LATERAL (SELECT COALESCE(` + counterBreak("p.", "p.prev_") + `, true)
	AS brk) b
WHERE p.prev_at IS NOT NULL AND p.collected_at >= $1
	AND p.collected_at - p.prev_at <= $3::interval
ORDER BY p.queryid, p.collected_at`

// hourCoverageSQL reads the hour [$1, $2)'s coverage inputs: the number of
// queries snapshots, whether every one has a recorded statements epoch,
// and the same-epoch sums of the database's statement time and of
// pg_stat_statements' dealloc over the system snapshots (NULL: unknown).
// Pairs are at most $3 (AnchorLookback) apart and end in the hour.
var hourCoverageSQL = `/* pg_sage */ WITH y AS (
	SELECT s.collected_at, d.doc->>'statements_epoch' IS NOT NULL AS known,
		(d.doc->>'statements_epoch')::timestamptz AS stats_epoch,
		(d.doc->>'statements_calls')::int8 AS calls,
		(d.doc->>'statements_total_exec_ms')::float8 AS total_exec_time,
		CASE WHEN (d.doc->'stat_statements_usage'->>'dealloc')::int8 >= 0
			THEN (d.doc->'stat_statements_usage'->>'dealloc')::int8 END AS dealloc
	FROM sage.snapshots s CROSS JOIN LATERAL (SELECT ` + snapstore.DataSQL("s") + ` AS doc) d
	WHERE {db:s} AND s.category = 'system'
		AND s.collected_at >= $1::timestamptz - $3::interval AND s.collected_at < $2
), p AS (
	SELECT y.*, lag(y.collected_at) OVER w AS prev_at,
		lag(y.stats_epoch) OVER w AS prev_stats_epoch, lag(y.calls) OVER w AS prev_calls,
		lag(y.total_exec_time) OVER w AS prev_total_exec_time,
		lag(y.dealloc) OVER w AS prev_dealloc
	FROM y WINDOW w AS (ORDER BY y.collected_at)
), pairs AS (
	SELECT p.*, (p.prev_at IS NOT NULL AND p.collected_at - p.prev_at <= $3::interval
		AND NOT ` + EpochBreak("p.", "p.prev_") + `) AS same
	FROM p WHERE p.collected_at >= $1
), snaps AS (
	SELECT count(*)::int AS n, COALESCE(bool_and(COALESCE(e.known, false)), false) AS known
	FROM sage.snapshots s
	LEFT JOIN y e ON e.collected_at = s.collected_at
	WHERE {db:s} AND s.category = 'queries' AND s.collected_at >= $1 AND s.collected_at < $2
)
SELECT snaps.n, snaps.known,
	(SELECT sum(total_exec_time - prev_total_exec_time) FROM pairs WHERE same),
	(SELECT sum(dealloc - prev_dealloc)::int8 FROM pairs
		WHERE same AND dealloc >= prev_dealloc)
FROM snaps`

// upsertHourlySQL writes one hour's rows ($1) from parallel arrays, each
// stamped with the newest plan fingerprint the query store captured for
// its queryid inside the hour (NULL beyond the store's retention). A
// re-rolled hour replaces its rows.
const upsertHourlySQL = `/* pg_sage */ INSERT INTO sage.query_rollup_hourly AS r
	(queryid, hour, calls, exec_ms, rows, shared_blks_hit, shared_blks_read,
	 temp_blks_read, temp_blks_written, wal_bytes, plan_hash, epoch_break, pairs{dbcol})
	SELECT u.queryid, $1::timestamptz, u.calls, u.exec_ms, u.rows, u.hit, u.read,
		u.tread, u.twrite, u.wal, ph.plan_hash, u.brk, u.pairs{dbval}
	FROM unnest($2::int8[], $3::int8[], $4::float8[], $5::int8[], $6::int8[], $7::int8[],
		$8::int8[], $9::int8[], $10::int8[], $11::bool[], $12::int2[])
		AS u(queryid, calls, exec_ms, rows, hit, read, tread, twrite, wal, brk, pairs)
	LEFT JOIN LATERAL (
		SELECT q.plan_hash FROM sage.query_store q
		WHERE {db:q} AND q.queryid = u.queryid AND q.plan_hash IS NOT NULL
			AND q.captured_at >= $1::timestamptz
			AND q.captured_at < $1::timestamptz + interval '1 hour'
		ORDER BY q.captured_at DESC, q.id DESC LIMIT 1) ph ON true
	ON CONFLICT ((COALESCE(database_id, 0)), queryid, hour) DO UPDATE SET
		calls = EXCLUDED.calls, exec_ms = EXCLUDED.exec_ms, rows = EXCLUDED.rows,
		shared_blks_hit = EXCLUDED.shared_blks_hit,
		shared_blks_read = EXCLUDED.shared_blks_read,
		temp_blks_read = EXCLUDED.temp_blks_read,
		temp_blks_written = EXCLUDED.temp_blks_written, wal_bytes = EXCLUDED.wal_bytes,
		plan_hash = EXCLUDED.plan_hash, epoch_break = EXCLUDED.epoch_break,
		pairs = EXCLUDED.pairs, built_at = now()`

// dropStaleHourlySQL removes the hour's rows of queryids a re-roll no
// longer finds (a late or removed snapshot), so a re-roll is exact.
const dropStaleHourlySQL = `/* pg_sage */ DELETE FROM sage.query_rollup_hourly r
	WHERE {db:r} AND r.hour = $1 AND r.queryid <> ALL($2::int8[])`

// upsertCoverageSQL writes the hour's coverage row.
const upsertCoverageSQL = `/* pg_sage */ INSERT INTO sage.query_rollup_coverage
	(hour, db_exec_ms, sampled_exec_ms, sampled_queryids, epoch_breaks, dealloc_delta,
	 snapshots, epoch_source{dbcol})
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8{dbval})
	ON CONFLICT ((COALESCE(database_id, 0)), hour) DO UPDATE SET
		db_exec_ms = EXCLUDED.db_exec_ms, sampled_exec_ms = EXCLUDED.sampled_exec_ms,
		sampled_queryids = EXCLUDED.sampled_queryids, epoch_breaks = EXCLUDED.epoch_breaks,
		dealloc_delta = EXCLUDED.dealloc_delta, snapshots = EXCLUDED.snapshots,
		epoch_source = EXCLUDED.epoch_source, built_at = now()`

// rollupResumeSQL is the newest rolled hour, and the oldest queries
// snapshot (where a first backfill starts).
const rollupResumeSQL = `/* pg_sage */ SELECT
	(SELECT max(c.hour) FROM sage.query_rollup_coverage c WHERE {db:c}),
	(SELECT min(s.collected_at) FROM sage.snapshots s
	  WHERE {db:s} AND s.category = 'queries')`
