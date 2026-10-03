package schema

import "strings"

// SRE performance fix (v1.8.3, reviews/2026-10-03-perf-sre-report.md): the
// performance gate found pg_sage reading whole history tables where it
// needs a handful of rows. These indexes make each read an index lookup
// or range bounded by what it returns:
//
//   - idx_decision_autonomy_execute: the earned-autonomy reconcile reads
//     the self-initiated family executions only (a hash join over every
//     executed decision of the ledger before).
//   - idx_action_queue_autonomy: the reconcile's L2 handoffs (autonomy:*
//     identities) without reading the approval queue's history.
//   - idx_verification_watch: a watch is looked up by its id inside the
//     baseline document.
//   - idx_incidents_autovacuum_cancel: the autovacuum-cancellation probe
//     reads cancellation incidents only. Keyed on columns the incident
//     writer never updates, so its updates stay HOT.
//   - idx_sre_investigations_created / _case: every investigation list
//     page (with or without a case) is an ordered range of its scope.
//
// sage.sre_sli_samples gains running per-series counters: each sample
// carries its series' reset-compensated bad and eligible totals, sample
// count and reset count since the chain started (chain_start), so an SLO
// window is the difference of two samples, not a re-aggregation of every
// sample in it. Samples stored before (NULL counters) are aggregated raw
// until they age out.
//
// A definition is a format() string: %I is the index name and %% a
// literal percent sign. Like the ledger migration, every step checks the
// catalog first: a re-run takes no lock (a bare CREATE INDEX IF NOT EXISTS
// or ALTER TABLE waits for a SHARE lock even when there is nothing to do).
// Idempotent.
var sreperfIndexSpecs = []ledgerIndex{
	{"idx_decision_autonomy_execute", "decision", "INDEX %I ON sage.decision (id) " +
		"WHERE verdict = 'execute' AND evidence ? 'incident_family'"},
	{"idx_action_queue_autonomy", "action_queue", "INDEX %I ON sage.action_queue " +
		"(action_log_id) WHERE identity_key LIKE 'autonomy:%%'"},
	{"idx_verification_watch", "verification",
		"INDEX %I ON sage.verification ((baseline->>'watch_id'))"},
	{"idx_incidents_autovacuum_cancel", "incidents", "INDEX %I ON sage.incidents " +
		"(detected_at) WHERE 'log_autovacuum_cancel' = ANY (signal_ids)"},
	{"idx_sre_investigations_created", "sre_investigations",
		"INDEX %I ON sage.sre_investigations " +
			"(deployment_id, database_id, created_at DESC, id DESC)"},
	{"idx_sre_investigations_case", "sre_investigations",
		"INDEX %I ON sage.sre_investigations " +
			"(deployment_id, database_id, source_case_id, created_at DESC, id DESC)"},
}

const ddlSLIRunningCounters = `
    IF (SELECT count(*) FROM pg_attribute
         WHERE attrelid = 'sage.sre_sli_samples'::regclass AND NOT attisdropped
           AND attname IN ('cum_bad', 'cum_eligible', 'cum_samples', 'cum_resets',
                           'chain_start')) < 5 THEN
        ALTER TABLE sage.sre_sli_samples
            ADD COLUMN IF NOT EXISTS cum_bad double precision,
            ADD COLUMN IF NOT EXISTS cum_eligible double precision,
            ADD COLUMN IF NOT EXISTS cum_samples bigint,
            ADD COLUMN IF NOT EXISTS cum_resets bigint,
            ADD COLUMN IF NOT EXISTS chain_start timestamptz;
    END IF;`

// ddlSREPerf is the migration: the SLI columns, then the indexes.
func ddlSREPerf() string {
	values := make([]string, 0, len(sreperfIndexSpecs))
	for _, index := range sreperfIndexSpecs {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	// The template's %% are its own; a definition's %% stay for format().
	loop := strings.Replace(strings.ReplaceAll(ddlDecisionLedgerIndexLoop, "%%", "%"),
		"%s", strings.Join(values, ",\n            "), 1)
	return "DO $$\nDECLARE spec record;\nBEGIN" + ddlSLIRunningCounters + loop + "\nEND $$;"
}
