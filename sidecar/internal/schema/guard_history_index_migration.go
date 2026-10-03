package schema

import "strings"

// Schema guard history read (dogfood lifeos, v1.8.3): 252,974 legacy rows
// from the v1.8.1 flood each named up to 71 leaked schemas, so a lookup by
// target through the GIN index over every schema guard row matched nearly
// all of them and the read took 12.7 s per scan. The read now needs:
//
//   - idx_decision_schema_guard_key: the newest row of each scanned
//     invariant identity, one index probe per identity however many rows
//     the identity has.
//   - idx_decision_schema_guard_counted: the rows the read counts (retention
//     dry runs and external reversions) by target. Recommendations and
//     parks, the whole flood, are not in it.
//
// idx_decision_schema_guard_targets served only the old read and is
// retired (dropped once, only while the catalog still has it).
//
// Same procedure as the decision ledger migration (catalog checked first,
// INVALID rebuilt, plain CREATE under the bootstrap lock: a CONCURRENTLY
// build beside the bootstrap deadlocked a fleet reload). A build pauses
// pg_sage's own writes to sage.decision once, at startup. Idempotent.
var guardHistoryIndexes = []ledgerIndex{
	{"idx_decision_schema_guard_key", "decision", "INDEX %I ON sage.decision " +
		"((evidence->>'invariant_key'), id DESC) WHERE feature = 'schema_guard'"},
	{"idx_decision_schema_guard_counted", "decision", "INDEX %I ON sage.decision " +
		"USING gin (target_objects) WHERE feature = 'schema_guard' AND " +
		"(evidence->>'disposition' = 'dry_run' OR " +
		"evidence->>'external_reversion' = 'true')"},
}

const retiredGuardTargetsIndex = "idx_decision_schema_guard_targets"

// ddlGuardHistoryIndexes is the migration: the checked index loop, then
// the retired index.
func ddlGuardHistoryIndexes() string {
	values := make([]string, 0, len(guardHistoryIndexes))
	for _, index := range guardHistoryIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	drop := "\n    IF EXISTS (SELECT 1 FROM pg_catalog.pg_indexes WHERE schemaname = " +
		"'sage' AND indexname = " + sqlLiteral(retiredGuardTargetsIndex) + ") THEN\n" +
		"        DROP INDEX sage." + retiredGuardTargetsIndex + ";\n    END IF;"
	return "DO $$\nDECLARE spec record;\nBEGIN" + loop + drop + "\nEND $$;"
}
