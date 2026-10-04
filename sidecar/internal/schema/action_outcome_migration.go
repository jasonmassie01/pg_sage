package schema

import "strings"

// Phase 1.3 (real verification): sage.action_outcome holds, per executed
// action, the predicted effect recorded before it ran and the verdict
// (improved, neutral, regressed, insufficient_evidence, unverifiable)
// with what was observed. It is the ledger the trust system reads. Rows
// follow their action (ON DELETE CASCADE), so action_log retention
// prunes them; the primary key serves the per-action reads and the
// cascade.
//
// The table is created only when the catalog lacks it: CREATE TABLE ...
// REFERENCES locks sage.action_log, so a re-run must not issue it. The
// index uses the decision ledger's checked loop (catalog first, INVALID
// rebuilt). Idempotent.
const ddlActionOutcomeTable = `
    IF to_regclass('sage.action_outcome') IS NULL THEN
        CREATE TABLE sage.action_outcome (
            action_log_id     bigint PRIMARY KEY
                              REFERENCES sage.action_log(id) ON DELETE CASCADE,
            database_id       bigint,
            action_class      text NOT NULL,
            predicted         jsonb NOT NULL,
            prediction_method text NOT NULL,
            verdict           text NOT NULL DEFAULT 'pending',
            tolerance         text NOT NULL DEFAULT 'pending',
            observed          jsonb NOT NULL DEFAULT '{}'::jsonb,
            evidence          jsonb NOT NULL DEFAULT '{}'::jsonb,
            reason            text NOT NULL DEFAULT '',
            window_start      timestamptz,
            window_end        timestamptz,
            created_at        timestamptz NOT NULL DEFAULT now(),
            decided_at        timestamptz,
            CONSTRAINT action_outcome_verdict CHECK (verdict IN ('pending', 'improved',
                'neutral', 'regressed', 'insufficient_evidence', 'unverifiable')),
            CONSTRAINT action_outcome_tolerance CHECK (tolerance IN ('pending', 'met',
                'partial', 'missed', 'no_prediction', 'unmeasured')),
            CONSTRAINT action_outcome_method CHECK (prediction_method IN ('hypopg',
                'model', 'rule', 'none'))
        );
    END IF;`

// actionOutcomeIndexes: the trust system reads outcomes per action class,
// newest decision first.
var actionOutcomeIndexes = []ledgerIndex{
	{"idx_action_outcome_class_decided", "action_outcome", "INDEX %I ON " +
		"sage.action_outcome (action_class, decided_at DESC)"},
}

// ddlActionOutcome is the migration: the table, then the checked index loop.
func ddlActionOutcome() string {
	values := make([]string, 0, len(actionOutcomeIndexes))
	for _, index := range actionOutcomeIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return "DO $$\nDECLARE spec record;\nBEGIN" + ddlActionOutcomeTable + loop + "\nEND $$;"
}
