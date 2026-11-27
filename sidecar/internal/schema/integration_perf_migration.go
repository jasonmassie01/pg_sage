package schema

import "strings"

// Perf gate on the merged tranches (improve/integration, 2026-11-24), two
// offenders:
//
//   - idx_action_log_last_failure: the executor's last-failure gauge reads
//     max(COALESCE(measured_at, executed_at)) of the failed, partial and
//     rollback_failed actions on every metrics refresh, a seq scan of the
//     whole ledger. The partial expression index answers it with one
//     backward index step. Its columns (outcome, measured_at,
//     executed_at) are already indexed, so it costs no HOT update.
//   - sage.workload_role_daily and sage.budget_shadow (market-08): every
//     upsert sets updated_at, and an index keyed it (gate F: 0 of 7
//     updates HOT). Retention ages their rows by day instead (a row's day
//     is never updated, and it is the UTC day its updates fall in), so the
//     updated_at indexes are retired and day is indexed.
//
// Same procedure as the self-exclusion indexes (catalog checked first,
// INVALID rebuilt, built under the bootstrap lock); a retired index is
// dropped only while the catalog still has it. Idempotent.
var integrationPerfIndexes = []ledgerIndex{
	{"idx_action_log_last_failure", "action_log", "INDEX %I ON sage.action_log " +
		"((COALESCE(measured_at, executed_at))) " +
		"WHERE outcome IN ('failed', 'partial', 'rollback_failed')"},
	{"idx_workload_role_daily_day", "workload_role_daily",
		"INDEX %I ON sage.workload_role_daily (day)"},
	{"idx_budget_shadow_day", "budget_shadow", "INDEX %I ON sage.budget_shadow (day)"},
}

// retiredUpdatedAtIndexes keyed the upserted updated_at columns.
var retiredUpdatedAtIndexes = []string{"workload_role_daily_updated_at_idx",
	"budget_shadow_updated_at_idx"}

// ddlIntegrationPerfIndexes is the migration: the retired indexes, then
// the checked index loop.
func ddlIntegrationPerfIndexes() string {
	var drops strings.Builder
	for _, name := range retiredUpdatedAtIndexes {
		drops.WriteString("\n    IF EXISTS (SELECT 1 FROM pg_catalog.pg_indexes WHERE " +
			"schemaname = 'sage' AND indexname = " + sqlLiteral(name) + ") THEN\n" +
			"        DROP INDEX sage." + name + ";\n    END IF;")
	}
	return ledgerIndexBlock(drops.String(), integrationPerfIndexes)
}
