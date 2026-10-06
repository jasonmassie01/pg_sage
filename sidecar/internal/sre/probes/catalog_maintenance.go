package probes

import "fmt"

// Vacuum/wraparound and plan-regression probes.

var autovacuumWraparoundSQL = `/* pg_sage sre:autovacuum_wraparound v1 */
SELECT ` + fmt.Sprintf(relationName, "c.oid") + ` AS relation,
       pg_catalog.age(c.relfrozenxid)::int8 AS xid_age,
       pg_catalog.mxid_age(c.relminmxid)::int8 AS mxid_age,
       s.n_dead_tup::int8 AS n_dead_tup,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - s.last_autovacuum)::float8
           AS last_autovacuum_age_s,
       (SELECT pg_catalog.age(d.datfrozenxid)::int8 FROM pg_catalog.pg_database d
        WHERE d.datname = pg_catalog.current_database()) AS database_xid_age,
       pg_catalog.current_setting('autovacuum_freeze_max_age')::int8
           AS freeze_max_age
FROM pg_catalog.pg_class c
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'm', 't')
ORDER BY pg_catalog.age(c.relfrozenxid) DESC, c.oid
LIMIT $1`

var vacuumProgressSQL = `/* pg_sage sre:vacuum_progress v1 */
SELECT p.pid, ` + fmt.Sprintf(relationName, "p.relid") + ` AS relation,
       p.phase,
       p.heap_blks_total::int8 AS heap_blks_total,
       p.heap_blks_scanned::int8 AS heap_blks_scanned,
       p.heap_blks_vacuumed::int8 AS heap_blks_vacuumed,
       p.index_vacuum_count::int8 AS index_vacuum_count,
       COALESCE(a.backend_type = 'autovacuum worker', false) AS autovacuum,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.xact_start)::float8
           AS running_s
FROM pg_catalog.pg_stat_progress_vacuum p
LEFT JOIN pg_catalog.pg_stat_activity a ON a.pid = p.pid
WHERE p.datname = pg_catalog.current_database()
ORDER BY p.pid
LIMIT $1`

// planRegressionsSQL joins query_store.plan_hash flips with windowed
// latency: per sample, delta total time / delta calls against the
// previous sample of the same statistics epoch (resets are never
// differenced). The split is the latest plan flip, or the middle of the
// window when the plan did not change; "before" only counts samples of
// the previous plan. Queries without fingerprints are not reported.
// $3 scopes the read to one queryid (0 reads every statement).
const planRegressionsSQL = `/* pg_sage sre:plan_regressions v1 */
WITH s AS (
    SELECT q.id, q.queryid, q.captured_at, q.plan_hash,
           (q.calls - lag(q.calls) OVER w)::float8 AS d_calls,
           q.total_exec_time - lag(q.total_exec_time) OVER w AS d_ms,
           lag(q.plan_hash) OVER w AS prev_hash,
           q.stats_epoch IS NOT DISTINCT FROM lag(q.stats_epoch) OVER w AS same_epoch
    FROM sage.query_store q
    WHERE q.captured_at >= pg_catalog.now()
              - pg_catalog.make_interval(secs => $2)
      AND q.plan_hash IS NOT NULL
      AND ($3::int8 = 0 OR q.queryid = $3::int8)
    WINDOW w AS (PARTITION BY q.queryid ORDER BY q.captured_at, q.id)
), flip AS (
    SELECT DISTINCT ON (queryid) queryid, captured_at AS flipped_at, prev_hash
    FROM s WHERE prev_hash IS NOT NULL AND prev_hash <> plan_hash
    ORDER BY queryid, captured_at DESC, id DESC
), span AS (
    SELECT queryid, min(captured_at) AS t0, max(captured_at) AS t1,
           (pg_catalog.array_agg(plan_hash ORDER BY captured_at DESC, id DESC))[1]
               AS last_hash
    FROM s GROUP BY queryid
), split AS (
    SELECT sp.queryid, sp.last_hash, f.flipped_at, f.prev_hash,
           COALESCE(f.flipped_at, sp.t0 + (sp.t1 - sp.t0) / 2) AS split_at
    FROM span sp LEFT JOIN flip f ON f.queryid = sp.queryid
), seg AS (
    SELECT x.queryid,
           sum(s.d_calls) FILTER (WHERE s.captured_at < x.split_at
               AND (x.flipped_at IS NULL OR s.plan_hash = x.prev_hash)) AS b_calls,
           sum(s.d_ms) FILTER (WHERE s.captured_at < x.split_at
               AND (x.flipped_at IS NULL OR s.plan_hash = x.prev_hash)) AS b_ms,
           sum(s.d_calls) FILTER (WHERE s.captured_at >= x.split_at) AS a_calls,
           sum(s.d_ms) FILTER (WHERE s.captured_at >= x.split_at) AS a_ms
    FROM split x
    JOIN s ON s.queryid = x.queryid
        AND s.d_calls > 0 AND s.d_ms >= 0 AND s.same_epoch
    GROUP BY x.queryid
)
SELECT x.queryid,
       x.prev_hash AS previous_plan_hash,
       x.last_hash AS current_plan_hash,
       x.flipped_at IS NOT NULL AS plan_flipped,
       x.flipped_at,
       g.b_calls::int8 AS before_calls,
       g.b_ms / g.b_calls AS before_mean_ms,
       g.a_calls::int8 AS after_calls,
       g.a_ms / g.a_calls AS after_mean_ms
FROM split x JOIN seg g ON g.queryid = x.queryid
WHERE g.b_calls > 0 AND g.a_calls > 0
ORDER BY (g.a_ms / g.a_calls) / NULLIF(g.b_ms / g.b_calls, 0) DESC NULLS LAST,
         x.queryid
LIMIT $1`

func autovacuumWraparoundSpec() Spec {
	return spec(AutovacuumWraparound, FamilyVacuum, ArgsNone,
		Variant{MinVersion: 140000, SQL: autovacuumWraparoundSQL})
}

func vacuumProgressSpec() Spec {
	return needsStats(spec(VacuumProgress, FamilyVacuum, ArgsNone,
		Variant{MinVersion: 140000, SQL: vacuumProgressSQL}))
}

func planRegressionsSpec() Spec {
	s := spec(PlanRegressions, FamilyPlans, ArgsWindow,
		Variant{MinVersion: 140000, SQL: planRegressionsSQL})
	s.QueryScoped = true
	return s
}
