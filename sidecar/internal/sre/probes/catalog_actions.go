package probes

// Action probes (Sage SRE M5): the fixed, read-only queries the action
// service runs around an approved backend cancel. They live in their own
// registry, so the model, which picks only from the diagnostic Catalog,
// can never run them. Like every probe they return no query text and no
// application name: only identities, states and hashes.

// Action probe ids.
const (
	SignalTarget   ID = "signal_target"
	RecoverySample ID = "recovery_sample"
)

// signalTargetSQL is the full identity of one session (pid plus
// backend_start) right now: what an approved cancel is matched against.
// The query and application name are hashed (sha256 of the UTF-8 text).
const signalTargetSQL = `/* pg_sage sre:signal_target v1 */
SELECT a.pid, a.backend_start, a.query_start, a.xact_start,
       a.datname, a.usename, a.state, a.backend_type,
       COALESCE(a.wait_event_type = 'Lock', false) AS waiting,
       a.query_id,
       pg_catalog.encode(pg_catalog.sha256(convert_to(a.query::text, 'UTF8')), 'hex')
           AS query_hash,
       (SELECT count(*) FROM pg_catalog.pg_stat_activity w
         WHERE a.pid = ANY (pg_catalog.pg_blocking_pids(w.pid)))::int8 AS blocking,
       pg_catalog.pg_is_in_recovery() AS in_recovery,
       a.datname IS NOT DISTINCT FROM pg_catalog.current_database() AS in_current_database,
       (a.application_name ILIKE '%pg_sage%' OR a.application_name ILIKE '%pg_dump%'
        OR a.application_name ILIKE '%pg_basebackup%'
        OR a.application_name ILIKE '%pg_restore%') AS protected_application,
       pg_catalog.encode(pg_catalog.sha256(convert_to(a.application_name::text,
           'UTF8')), 'hex') AS application_hash
FROM pg_catalog.pg_stat_activity a
WHERE a.pid = $2 AND a.backend_start = $3
  AND a.pid <> pg_catalog.pg_backend_pid()
LIMIT $1`

// recoverySampleSQL lists this database's client sessions with whether
// each waits on a lock and whether the target session (pid plus
// backend_start) blocks it: the blocking family's recovery predicate.
const recoverySampleSQL = `/* pg_sage sre:recovery_sample v1 */
SELECT a.pid, a.backend_start, a.state,
       COALESCE(a.wait_event_type = 'Lock', false) AS waiting,
       ($2::int4 = ANY (pg_catalog.pg_blocking_pids(a.pid))) AS blocked_by_target,
       (a.pid = $2::int4 AND a.backend_start = $3) AS is_target
FROM pg_catalog.pg_stat_activity a
WHERE a.datname = pg_catalog.current_database()
  AND a.backend_type = 'client backend'
  AND a.pid <> pg_catalog.pg_backend_pid()
  AND a.application_name NOT ILIKE '%pg_sage%'
ORDER BY a.pid
LIMIT $1`

func signalTargetSpec() Spec {
	return spec(SignalTarget, FamilyLocks, ArgsBackend, Variant{MinVersion: 140000,
		SQL: signalTargetSQL})
}

func recoverySampleSpec() Spec {
	return spec(RecoverySample, FamilyLocks, ArgsBackend, Variant{MinVersion: 140000,
		SQL: recoverySampleSQL})
}

// actionRegistry is the validated action probe registry, built once.
var actionRegistry = mustRegistry(signalTargetSpec(), recoverySampleSpec())

// ActionRegistry returns the action probes (not the diagnostic catalog).
func ActionRegistry() *Registry { return actionRegistry }
