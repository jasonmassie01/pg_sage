package schema

import "strings"

// Decision ledger (dogfood lifeos, perf audit F1/F8). sage.decision grew by
// ~41,000 rows an hour: the standing gate inserted a row on every
// evaluation and the schema guard one per invariant per cycle.
//
//   - A non-execute verdict carries a fingerprint; repeats of an open
//     fingerprint update one row (repeat_count, last_seen_at). The unique
//     index backs that upsert (INSERT ... ON CONFLICT).
//   - idx_decision_schema_guard_targets serves the schema guard's history
//     lookup by target (the coordinator created it by hand on lifeos with
//     this exact definition; an existing index is kept).
//   - idx_decision_created serves retention's age-based purge (the only
//     created_at index led with database_id, NULL on every lifeos row).
//   - The rest give every foreign key into or out of sage.decision a
//     leading index, so purges and retention's keep anti-joins never scan.
//
// The indexes are built here, under the bootstrap lock, not CONCURRENTLY
// beside it: a concurrent build waits for older snapshots while the
// existing CREATE INDEX IF NOT EXISTS migrations on sage.decision take a
// SHARE lock even when the index exists, which deadlocked a fleet reload.
// Each step checks the catalog first, so a re-run takes no lock on any
// table; a build pauses only pg_sage's own writes to that table, once, at
// startup. An INVALID index (a failed build by hand) is rebuilt unless a
// build is still running. Idempotent.

type ledgerIndex struct{ name, table, definition string }

var ledgerIndexes = []ledgerIndex{
	{"idx_decision_fingerprint", "decision", "UNIQUE INDEX %I ON sage.decision " +
		"(fingerprint) WHERE fingerprint IS NOT NULL AND resolved_at IS NULL"},
	{"idx_decision_schema_guard_targets", "decision",
		"INDEX %I ON sage.decision USING gin (target_objects) " +
			"WHERE feature = 'schema_guard'"},
	{"idx_decision_created", "decision", "INDEX %I ON sage.decision (created_at)"},
	{"idx_decision_action_log", "decision", "INDEX %I ON sage.decision " +
		"(action_log_id) WHERE action_log_id IS NOT NULL"},
	{"idx_decision_queue", "decision",
		"INDEX %I ON sage.decision (queue_id) WHERE queue_id IS NOT NULL"},
	{"idx_decision_policy", "decision",
		"INDEX %I ON sage.decision (policy_id) WHERE policy_id IS NOT NULL"},
	{"idx_verification_decision", "verification",
		"INDEX %I ON sage.verification (decision_id)"},
	{"idx_change_lease_decision", "change_lease",
		"INDEX %I ON sage.change_lease (decision_id)"},
	{"idx_incident_avoided_decision", "incident_avoided",
		"INDEX %I ON sage.incident_avoided (decision_id)"},
	{"idx_schema_baseline_decision", "schema_baseline",
		"INDEX %I ON sage.schema_baseline (last_authorized_decision_id) " +
			"WHERE last_authorized_decision_id IS NOT NULL"},
}

const ddlDecisionLedgerColumns = `
    IF (SELECT count(*) FROM pg_attribute
         WHERE attrelid = 'sage.decision'::regclass AND NOT attisdropped
           AND attname IN ('fingerprint', 'repeat_count', 'last_seen_at')) < 3 THEN
        ALTER TABLE sage.decision
            ADD COLUMN IF NOT EXISTS fingerprint text,
            ADD COLUMN IF NOT EXISTS repeat_count integer NOT NULL DEFAULT 1,
            ADD COLUMN IF NOT EXISTS last_seen_at timestamptz;
    END IF;`

const ddlDecisionLedgerIndexLoop = `
    FOR spec IN SELECT * FROM (VALUES %s) AS v(name, tbl, definition) LOOP
        CONTINUE WHEN to_regclass('sage.' || spec.tbl) IS NULL;
        IF EXISTS (SELECT 1 FROM pg_index i
                    WHERE i.indexrelid = to_regclass('sage.' || spec.name)
                      AND NOT i.indisvalid
                      AND NOT EXISTS (SELECT 1 FROM pg_stat_progress_create_index p
                                       WHERE p.index_relid = i.indexrelid)) THEN
            EXECUTE format('DROP INDEX sage.%%I', spec.name);
        END IF;
        IF to_regclass('sage.' || spec.name) IS NULL THEN
            EXECUTE format('CREATE ' || spec.definition, spec.name);
        END IF;
    END LOOP;`

// ddlDecisionLedger is the ledger migration: columns, then indexes.
func ddlDecisionLedger() string {
	values := make([]string, 0, len(ledgerIndexes))
	for _, index := range ledgerIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return "DO $$\nDECLARE spec record;\nBEGIN" + ddlDecisionLedgerColumns + loop +
		"\nEND $$;"
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
