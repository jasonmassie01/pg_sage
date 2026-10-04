package schema

import "strings"

// Roadmap 1.2 (one trust system, 2026-10-03). The earned-autonomy ledger
// also governs every self-initiated action class (trust families tuning
// and hygiene). Its outcomes keep the raw verdict of sage.action_outcome
// (or "rolled_back" / "rejected"), when that was observed in the
// monitored database and, for an operator rejection, the approval queue
// item; results gain "rejected" (an operator rollback or rejection) and
// sources "rollback". Levels gain the "grandfathered" provenance (the
// autonomy the time ramp granted on the day the ledger took over) and the
// history its event. sage.trust_ledger_state holds, per database, the
// once-only grandfathering marker and the reconcile cursors.
//
// Additive and idempotent. The constraints the earlier M7 migrations
// create keep their names and are redefined in place only when they lack
// the new values (those migrations re-run first and skip existing names).
const ddlTrustLedgerColumns = `
ALTER TABLE sage.sre_autonomy_outcomes
    ADD COLUMN IF NOT EXISTS verdict text,
    ADD COLUMN IF NOT EXISTS observed_at timestamptz,
    ADD COLUMN IF NOT EXISTS queue_id bigint;

CREATE TABLE IF NOT EXISTS sage.trust_ledger_state (
    deployment_id     uuid NOT NULL,
    database_name     text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    grandfathered_at  timestamptz,
    bound             jsonb,
    seeded            jsonb,
    verdict_cursor    timestamptz,
    rollback_cursor   timestamptz,
    rejection_cursor  timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_name)
);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_outcomes_result_v2'
                     AND conrelid = 'sage.sre_autonomy_outcomes'::regclass
                     AND pg_get_constraintdef(oid) LIKE '%rejected%') THEN
        ALTER TABLE sage.sre_autonomy_outcomes
            DROP CONSTRAINT IF EXISTS sre_autonomy_outcomes_result_v2;
        ALTER TABLE sage.sre_autonomy_outcomes
            ADD CONSTRAINT sre_autonomy_outcomes_result_v2 CHECK (result IN
                ('verified_recovery', 'not_recovered', 'harmful', 'safety_violation',
                 'unverified', 'rejected'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_outcomes_source_v2'
                     AND conrelid = 'sage.sre_autonomy_outcomes'::regclass) THEN
        ALTER TABLE sage.sre_autonomy_outcomes
            DROP CONSTRAINT IF EXISTS sre_autonomy_outcomes_source_check;
        ALTER TABLE sage.sre_autonomy_outcomes
            ADD CONSTRAINT sre_autonomy_outcomes_source_v2 CHECK (source IN
                ('executor', 'operator', 'game_day', 'rollout', 'bench', 'rollback'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_outcomes_verdict'
                     AND conrelid = 'sage.sre_autonomy_outcomes'::regclass) THEN
        ALTER TABLE sage.sre_autonomy_outcomes
            ADD CONSTRAINT sre_autonomy_outcomes_verdict CHECK (verdict IS NULL OR verdict IN
                ('improved', 'neutral', 'regressed', 'insufficient_evidence',
                 'unverifiable', 'rolled_back', 'rejected'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_family_autonomy_provenance'
                     AND conrelid = 'sage.sre_family_autonomy'::regclass
                     AND pg_get_constraintdef(oid) LIKE '%grandfathered%') THEN
        ALTER TABLE sage.sre_family_autonomy
            DROP CONSTRAINT IF EXISTS sre_family_autonomy_provenance;
        ALTER TABLE sage.sre_family_autonomy
            ADD CONSTRAINT sre_family_autonomy_provenance CHECK (
                (provenance = 'ledger' AND carried_ref IS NULL) OR
                (provenance IN ('carried_over', 'grandfathered') AND carried_ref IS NOT NULL
                 AND length(carried_ref) BETWEEN 1 AND 500));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_events_type_v2'
                     AND conrelid = 'sage.sre_autonomy_events'::regclass
                     AND pg_get_constraintdef(oid) LIKE '%grandfathered%') THEN
        ALTER TABLE sage.sre_autonomy_events
            DROP CONSTRAINT IF EXISTS sre_autonomy_events_type_v2;
        ALTER TABLE sage.sre_autonomy_events
            ADD CONSTRAINT sre_autonomy_events_type_v2 CHECK (event_type IN (
                'promotion_proposed', 'promotion_approved', 'promotion_rejected',
                'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
                'cap_cleared', 'auto_executed', 'carried_over', 'deadline_override',
                'database_scoped', 'grandfathered'));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS sre_autonomy_outcome_per_queue
    ON sage.sre_autonomy_outcomes (deployment_id, database_name, queue_id)
    WHERE queue_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sre_autonomy_outcomes_db_pair
    ON sage.sre_autonomy_outcomes (deployment_id, database_name, family, action_class);
`

// trustLedgerIndexes serve the reconciler's incremental reads in the
// monitored database: verdicts by decision time, operator rejections by
// decision time (operator rollbacks use idx_action_log_rolled_back).
var trustLedgerIndexes = []ledgerIndex{
	{"idx_action_outcome_decided", "action_outcome", "INDEX %I ON " +
		"sage.action_outcome (decided_at) WHERE decided_at IS NOT NULL"},
	{"idx_action_queue_rejected", "action_queue", "INDEX %I ON sage.action_queue " +
		"(decided_at) WHERE status = 'rejected'"},
}

// ddlTrustLedger is the migration: columns, table and constraints, then
// the checked index loop (catalog first, INVALID rebuilt).
func ddlTrustLedger() string {
	values := make([]string, 0, len(trustLedgerIndexes))
	for _, index := range trustLedgerIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return ddlTrustLedgerColumns + "DO $$\nDECLARE spec record;\nBEGIN" + loop + "\nEND $$;"
}
