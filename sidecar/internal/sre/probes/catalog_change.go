package probes

// M2 probes: the WAL archiver and pg_sage's own recent actions (the
// change feed's first source: "did pg_sage cause this?").

const archiverSQL = `/* pg_sage sre:archiver v1 */
SELECT pg_catalog.current_setting('archive_mode') AS archive_mode,
       a.archived_count::int8 AS archived_count,
       a.failed_count::int8 AS failed_count,
       a.last_archived_time, a.last_failed_time, a.stats_reset
FROM pg_catalog.pg_stat_archiver a
LIMIT $1`

// sageActionsSQL reads pg_sage's own action history in the window. It
// returns identities, types, outcomes and ages, never the SQL text.
const sageActionsSQL = `/* pg_sage sre:sage_actions v1 */
SELECT l.id::int8 AS id, pg_catalog.left(l.action_type, 64) AS action_type,
       pg_catalog.left(l.outcome, 32) AS outcome, l.executed_at,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - l.executed_at)::float8 AS age_s
FROM sage.action_log l
WHERE l.executed_at > pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => $2)
ORDER BY l.executed_at DESC, l.id DESC
LIMIT $1`

func archiverSpec() Spec {
	return spec(Archiver, FamilyWAL, ArgsNone,
		Variant{MinVersion: 140000, SQL: archiverSQL})
}

func sageActionsSpec() Spec {
	return spec(SageActions, FamilyChange, ArgsWindow,
		Variant{MinVersion: 140000, SQL: sageActionsSQL})
}
