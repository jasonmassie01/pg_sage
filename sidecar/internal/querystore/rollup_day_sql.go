package querystore

// The daily tier's and the coverage report's statements (evid-01b). Each
// runs through a history Store (histstore markers).

// rollupDaySQL sums the hourly rows of [$2, $3) per queryid into day $1
// and returns the queryids found and the daily rows written. A conflicting
// row is replaced only by a sum over at least as many hours, so a day the
// purge already thinned keeps its row.
const rollupDaySQL = `/* pg_sage */ WITH src AS (
	SELECT h.queryid, sum(h.calls) AS calls, sum(h.exec_ms) AS exec_ms,
		sum(h.rows) AS rows, sum(h.shared_blks_hit) AS hit, sum(h.shared_blks_read) AS read,
		sum(h.temp_blks_read) AS tread, sum(h.temp_blks_written) AS twrite,
		sum(h.wal_bytes) AS wal, count(*)::int2 AS hours,
		(count(*) FILTER (WHERE h.epoch_break))::int2 AS breaks,
		count(DISTINCT h.plan_hash)::int2 AS plans,
		(array_agg(h.plan_hash ORDER BY h.hour DESC)
			FILTER (WHERE h.plan_hash IS NOT NULL))[1] AS last_plan
	FROM sage.query_rollup_hourly h
	WHERE {db:h} AND h.hour >= $2 AND h.hour < $3
	GROUP BY h.queryid
), ins AS (
	INSERT INTO sage.query_rollup_daily AS d (queryid, day, calls, exec_ms, rows,
		shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written, wal_bytes,
		hours, epoch_breaks, plan_hashes, last_plan_hash{dbcol})
	SELECT s.queryid, ($1::timestamptz AT TIME ZONE 'UTC')::date, s.calls, s.exec_ms,
		s.rows, s.hit, s.read, s.tread, s.twrite, s.wal, s.hours, s.breaks, s.plans,
		s.last_plan{dbval}
	FROM src s
	ON CONFLICT ((COALESCE(database_id, 0)), queryid, day) DO UPDATE SET
		calls = EXCLUDED.calls, exec_ms = EXCLUDED.exec_ms, rows = EXCLUDED.rows,
		shared_blks_hit = EXCLUDED.shared_blks_hit,
		shared_blks_read = EXCLUDED.shared_blks_read,
		temp_blks_read = EXCLUDED.temp_blks_read,
		temp_blks_written = EXCLUDED.temp_blks_written, wal_bytes = EXCLUDED.wal_bytes,
		hours = EXCLUDED.hours, epoch_breaks = EXCLUDED.epoch_breaks,
		plan_hashes = EXCLUDED.plan_hashes, last_plan_hash = EXCLUDED.last_plan_hash,
		built_at = now()
	WHERE EXCLUDED.hours >= d.hours
	RETURNING 1
)
SELECT (SELECT count(*)::int FROM src), (SELECT count(*) FROM ins)`

// dayResumeSQL is the newest rolled hour (a day is due once its last hour
// is) and the newest daily day.
const dayResumeSQL = `/* pg_sage */ SELECT
	(SELECT max(c.hour) FROM sage.query_rollup_coverage c WHERE {db:c}),
	(SELECT max(d.day)::timestamp AT TIME ZONE 'UTC' FROM sage.query_rollup_daily d WHERE {db:d})`

// nextHourlySQL is the oldest hourly row at or after $1.
const nextHourlySQL = `/* pg_sage */ SELECT min(h.hour) FROM sage.query_rollup_hourly h
	WHERE {db:h} AND h.hour >= $1`

// oldestQueriesSnapshotSQL is where a rebuild of the hourly tier starts.
const oldestQueriesSnapshotSQL = `/* pg_sage */ SELECT min(s.collected_at)
	FROM sage.snapshots s WHERE {db:s} AND s.category = 'queries'`

// coverageSQL sums the coverage rows of [$1, $2) and reads the history's
// oldest and newest hour and oldest day.
const coverageSQL = `/* pg_sage */ SELECT count(*)::int,
	(count(*) FILTER (WHERE c.db_exec_ms IS NOT NULL))::int,
	COALESCE(sum(c.sampled_exec_ms) FILTER (WHERE c.db_exec_ms IS NOT NULL), 0)::float8,
	COALESCE(sum(c.db_exec_ms), 0)::float8,
	(count(*) FILTER (WHERE c.epoch_breaks > 0))::int,
	(count(*) FILTER (WHERE c.sampled_queryids > 0 OR COALESCE(c.db_exec_ms, 0) > 0))::int,
	sum(c.dealloc_delta)::int8,
	(count(*) FILTER (WHERE c.epoch_source = 'counters_only'))::int,
	(SELECT min(a.hour) FROM sage.query_rollup_coverage a WHERE {db:a}),
	(SELECT max(a.hour) FROM sage.query_rollup_coverage a WHERE {db:a}),
	(SELECT min(d.day)::timestamp AT TIME ZONE 'UTC' FROM sage.query_rollup_daily d WHERE {db:d})
FROM sage.query_rollup_coverage c WHERE {db:c} AND c.hour >= $1 AND c.hour < $2`

// rollupBytesSQL is the on-disk size of the three rollup tables (monitored
// placement: they hold only this database's rows).
const rollupBytesSQL = `/* pg_sage */ SELECT (
	COALESCE(pg_total_relation_size(to_regclass('sage.query_rollup_hourly')), 0) +
	COALESCE(pg_total_relation_size(to_regclass('sage.query_rollup_daily')), 0) +
	COALESCE(pg_total_relation_size(to_regclass('sage.query_rollup_coverage')), 0))::int8`
