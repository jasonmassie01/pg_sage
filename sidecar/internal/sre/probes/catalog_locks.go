package probes

import "fmt"

// Lock and transaction probes. Every query is fixed, schema-qualified,
// scoped to current_database() (fleet isolation) and bounded by LIMIT $1.
// No probe returns query text: only identities, states, modes and ages.

// relationName renders a relation oid as schema.table, independent of
// the probe's fixed search_path.
const relationName = `(SELECT pg_catalog.quote_ident(rn.nspname) || '.' ||
        pg_catalog.quote_ident(rc.relname)
    FROM pg_catalog.pg_class rc
    JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
    WHERE rc.oid = %s)`

// lockChainsSQL is the M0 lock-chain walk (analyzer.ProbeLockChains)
// without query text: each root blocker with its chain depth and the
// number of sessions it blocks, directly or transitively.
const lockChainsSQL = `/* pg_sage sre:lock_chains v1 */
WITH RECURSIVE lock_chain AS (
    SELECT sa.pid AS blocked_pid, b.pid AS blocker_pid, 1 AS depth,
           ARRAY[sa.pid, b.pid] AS chain
    FROM pg_catalog.pg_stat_activity sa
    CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(sa.pid)) AS b(pid)
    WHERE sa.wait_event_type = 'Lock'
      AND sa.datname = pg_catalog.current_database()
    UNION ALL
    SELECT lc.blocked_pid, u.pid, lc.depth + 1, lc.chain || u.pid
    FROM lock_chain lc
    CROSS JOIN LATERAL pg_catalog.unnest(
        pg_catalog.pg_blocking_pids(lc.blocker_pid)) AS u(pid)
    WHERE u.pid <> ALL (lc.chain) AND lc.depth < 10
),
roots AS (
    SELECT DISTINCT blocker_pid AS root_pid FROM lock_chain
    WHERE blocker_pid NOT IN (SELECT blocked_pid FROM lock_chain)
)
SELECT r.root_pid,
       CASE WHEN r.root_pid = 0 THEN 'prepared_xact' ELSE 'backend' END AS root_kind,
       sa.state AS root_state,
       sa.backend_start AS root_backend_start,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - sa.xact_start)::float8
           AS root_xact_age_s,
       sa.query_id AS root_query_id,
       max(lc.depth)::int8 AS chain_depth,
       count(DISTINCT lc.blocked_pid)::int8 AS total_blocked
FROM roots r
JOIN lock_chain lc ON lc.blocker_pid = r.root_pid
    OR (r.root_pid = ANY (lc.chain) AND lc.blocked_pid <> r.root_pid)
LEFT JOIN pg_catalog.pg_stat_activity sa ON sa.pid = r.root_pid AND r.root_pid <> 0
GROUP BY r.root_pid, sa.state, sa.backend_start, sa.xact_start, sa.query_id
ORDER BY total_blocked DESC, r.root_pid
LIMIT $1`

// lockGraphSQL returns one row per wait edge: the waiting session, the
// lock it requests, and each session (or prepared transaction, pid 0)
// that blocks it.
var lockGraphSQL = `/* pg_sage sre:lock_graph v1 */
SELECT w.pid AS waiter_pid,
       w.backend_start AS waiter_backend_start,
       w.state AS waiter_state,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - w.xact_start)::float8
           AS waiter_xact_age_s,
       wl.locktype AS lock_type,
       wl.mode AS requested_mode,
       ` + fmt.Sprintf(relationName, "wl.relation") + ` AS relation,
       bp.pid AS blocker_pid,
       CASE WHEN bp.pid = 0 THEN 'prepared_xact' ELSE 'backend' END AS blocker_kind,
       b.backend_start AS blocker_backend_start,
       b.state AS blocker_state,
       COALESCE(b.wait_event_type = 'Lock', false) AS blocker_waiting,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - b.xact_start)::float8
           AS blocker_xact_age_s,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - b.state_change)::float8
           AS blocker_state_age_s,
       b.query_id AS blocker_query_id
FROM pg_catalog.pg_stat_activity w
CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(w.pid)) AS bp(pid)
LEFT JOIN pg_catalog.pg_stat_activity b ON b.pid = bp.pid AND bp.pid <> 0
LEFT JOIN LATERAL (
    SELECT l.locktype, l.mode, l.relation FROM pg_catalog.pg_locks l
    WHERE l.pid = w.pid AND NOT l.granted
    ORDER BY l.locktype LIMIT 1
) wl ON true
WHERE w.wait_event_type = 'Lock'
  AND w.datname = pg_catalog.current_database()
ORDER BY w.pid, bp.pid
LIMIT $1`

const longTransactionsSQL = `/* pg_sage sre:long_transactions v1 */
SELECT a.pid, a.backend_start, a.state,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.xact_start)::float8
           AS xact_age_s,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.state_change)::float8
           AS state_age_s,
       COALESCE(a.wait_event_type = 'Lock', false) AS waiting,
       a.query_id,
       pg_catalog.age(a.backend_xmin)::int8 AS backend_xmin_age
FROM pg_catalog.pg_stat_activity a
WHERE a.datname = pg_catalog.current_database()
  AND a.xact_start IS NOT NULL
  AND a.pid <> pg_catalog.pg_backend_pid()
  AND a.backend_type = 'client backend'
ORDER BY a.xact_start, a.pid
LIMIT $1`

const preparedXactsSQL = `/* pg_sage sre:prepared_xacts v1 */
SELECT pg_catalog.md5(p.gid) AS gid_hash,
       p.owner::text AS owner,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - p.prepared)::float8
           AS prepared_age_s,
       pg_catalog.age(p.transaction)::int8 AS xid_age
FROM pg_catalog.pg_prepared_xacts p
WHERE p.database = pg_catalog.current_database()
ORDER BY p.prepared
LIMIT $1`

const backendIdentitySQL = `/* pg_sage sre:backend_identity v1 */
SELECT a.pid, a.backend_start, a.state, a.backend_type,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.xact_start)::float8
           AS xact_age_s,
       a.query_id
FROM pg_catalog.pg_stat_activity a
WHERE a.pid = $2 AND a.backend_start = $3
  AND a.datname = pg_catalog.current_database()
LIMIT $1`

func lockChainsSpec() Spec {
	return spec(LockChains, FamilyLocks, ArgsNone, Variant{MinVersion: 140000,
		SQL: lockChainsSQL})
}

func lockGraphSpec() Spec {
	return spec(LockGraph, FamilyLocks, ArgsNone, Variant{MinVersion: 140000,
		SQL: lockGraphSQL})
}

func longTransactionsSpec() Spec {
	return spec(LongTransactions, FamilyLocks, ArgsNone, Variant{MinVersion: 140000,
		SQL: longTransactionsSQL})
}

func preparedXactsSpec() Spec {
	return spec(PreparedXacts, FamilyLocks, ArgsNone, Variant{MinVersion: 140000,
		SQL: preparedXactsSQL})
}

func backendIdentitySpec() Spec {
	return spec(BackendIdentity, FamilyLocks, ArgsBackend, Variant{MinVersion: 140000,
		SQL: backendIdentitySQL})
}
