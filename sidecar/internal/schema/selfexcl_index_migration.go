package schema

import "strings"

// perf v1.8.3 (claude/perf-selfexcl), two perf gate offenders:
//
//   - idx_action_log_value_credit: /value sums credited actions (a
//     verified success with toil_minutes_saved) by day and action type.
//     It used to read every successful action; the partial covering index
//     holds the credited ones only, so the read is an index-only range
//     whatever the ledger's size. Crediting is a one-time update.
//   - sage.sre_investigations: every lease grant, step, budget and case
//     update moves updated_at, and two indexes keyed it (43 % HOT updates
//     in the gate). The queue index is keyed on created_at instead (the
//     runnable list orders by it); the retention index is retired:
//     retention ranges over idx_sre_investigations_created (an
//     investigation unchanged since the cutoff was created before it).
//
// Same procedure as the decision ledger migration (catalog checked first,
// INVALID rebuilt, built under the bootstrap lock); a retired index is
// dropped only when the catalog still has it. Idempotent.
var selfexclIndexes = []ledgerIndex{
	{"idx_action_log_value_credit", "action_log", "INDEX %I ON sage.action_log " +
		"(executed_at) INCLUDE (action_type, toil_minutes_saved) " +
		"WHERE outcome = 'success' AND toil_minutes_saved IS NOT NULL"},
	{"idx_sre_investigations_queue", "sre_investigations", "INDEX %I ON " +
		"sage.sre_investigations (deployment_id, database_id, state, created_at)"},
}

// retiredInvestigationIndexes keyed updated_at.
var retiredInvestigationIndexes = []string{"sre_investigation_queue",
	"sre_investigation_retention"}

// ddlSelfExclIndexes is the migration: the checked index loop, then the
// retired indexes.
func ddlSelfExclIndexes() string {
	values := make([]string, 0, len(selfexclIndexes))
	for _, index := range selfexclIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	drops := ""
	for _, name := range retiredInvestigationIndexes {
		drops += "\n    IF EXISTS (SELECT 1 FROM pg_catalog.pg_indexes WHERE schemaname = " +
			"'sage' AND indexname = " + sqlLiteral(name) + ") THEN\n        DROP INDEX sage." +
			name + ";\n    END IF;"
	}
	return "DO $$\nDECLARE spec record;\nBEGIN" + loop + drops + "\nEND $$;"
}
