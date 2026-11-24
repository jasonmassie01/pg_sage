package schema

import (
	"strings"
	"testing"
)

// The perf gate step is the last T7 step and lists the gauge index, the
// day indexes and the retired updated_at indexes. No concurrent access
// test: the step is a constant string (the bootstrap lock serializes it;
// the DB test boots twice).
func TestIntegrationPerfStep_IsLastAndNamesItsIndexes(t *testing.T) {
	steps := trancheSevenMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlIntegrationPerfIndexes" || last[1] != ddlIntegrationPerfIndexes() {
		t.Fatalf("tranche seven ends %s, want ddlIntegrationPerfIndexes", last[0])
	}
	if steps[len(steps)-2][0] != "ddlApprovalPRs" {
		t.Fatalf("ddlIntegrationPerfIndexes follows %s, want ddlApprovalPRs",
			steps[len(steps)-2][0])
	}
	all := migrationSteps()
	if all[len(all)-1].name != "ddlIntegrationPerfIndexes" {
		t.Fatalf("migrationSteps ends %s, want ddlIntegrationPerfIndexes",
			all[len(all)-1].name)
	}
	sql := ddlIntegrationPerfIndexes()
	for _, want := range []string{
		"'idx_action_log_last_failure', 'action_log'",
		"((COALESCE(measured_at, executed_at))) " +
			"WHERE outcome IN (''failed'', ''partial'', ''rollback_failed'')",
		"'idx_workload_role_daily_day', 'workload_role_daily', " +
			"'INDEX %I ON sage.workload_role_daily (day)'",
		"'idx_budget_shadow_day', 'budget_shadow', 'INDEX %I ON sage.budget_shadow (day)'",
		"indexname = 'workload_role_daily_updated_at_idx'",
		"DROP INDEX sage.workload_role_daily_updated_at_idx;",
		"indexname = 'budget_shadow_updated_at_idx'",
		"DROP INDEX sage.budget_shadow_updated_at_idx;",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("ddlIntegrationPerfIndexes lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "%%") {
		t.Errorf("ddlIntegrationPerfIndexes keeps an escaped percent sign:\n%s", sql)
	}
}

// The gauge's predicate is the index's: a different IN list would not
// imply the partial index's predicate and the planner would scan the
// ledger again.
func TestIntegrationPerfStep_GaugePredicateMatchesTheIndex(t *testing.T) {
	def := integrationPerfIndexes[0].definition
	if !strings.HasSuffix(def, "WHERE outcome IN ('failed', 'partial', 'rollback_failed')") {
		t.Fatalf("idx_action_log_last_failure = %s", def)
	}
}
