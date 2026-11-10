// Package mutationcensus holds the maintained inventory of every place
// pg_sage's shipped Go source can write to a database it connects to.
//
// The package has no production code. Its tests parse internal/ and cmd/
// with go/ast and find two kinds of write sink:
//
//   - Rule A: a SQL string (a literal, a constant, a concatenation or a
//     fmt.Sprintf format) passed to Exec, Query, QueryRow, CopyFrom,
//     SendBatch or Batch.Queue whose statement starts with a write verb
//     (INSERT, UPDATE ... SET, DELETE, TRUNCATE, MERGE, CREATE, ALTER, DROP,
//     COMMENT ON, GRANT, REVOKE, VACUUM, ANALYZE, REINDEX, CLUSTER,
//     REFRESH MATERIALIZED) or that calls a function changing state other
//     sessions see (hypopg_create_index, pg_cancel_backend,
//     pg_terminate_backend, pg_stat_reset*, pg_stat_statements_reset,
//     pg_reload_conf, the replication-slot functions, pg_switch_wal,
//     pg_promote, pg_create_restore_point), unless its target (every
//     target, for a DROP, TRUNCATE, GRANT, REVOKE, VACUUM or ANALYZE
//     list) is in the sage schema;
//   - Rule B: any reference to executor.ExecConcurrently, ExecInTransaction
//     or ExecStatistics from outside internal/executor, whatever its SQL.
//
// Every sink must have a row in sinks.tsv, keyed path#Func, with a class:
// gate:<contracts> (runs only through Executor.Apply under those registered
// contracts), sage_schema (writes only pg_sage's own schema, which the scan
// could not prove from the SQL text), text (the SQL is shown to a person,
// never run), clone_only (runs only against a disposable clone or
// rehearsal database) or out_of_gate:<reason>. The test fails on an
// unlisted sink, a stale row and an unknown class, so a new write path
// cannot land without someone classifying it. See docs/security.md,
// "Writes to the monitored database".
package mutationcensus
