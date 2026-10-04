package executor

import "github.com/pg-sage/sidecar/internal/extstats"

// createStatisticsContract: owner decision 2026-10-04 (PR #110). pg_sage
// runs the pg_sage form of CREATE STATISTICS (extstats) and ANALYZE of the
// table in one transaction, recorded as one action; the ANALYZE is what
// builds the statistics, so it is part of the action, not a separate
// hygiene action. PostgreSQL takes SHARE UPDATE EXCLUSIVE on the table for
// both statements: reads and writes continue. The earned ledger decides
// approval (tuning family, class statistics), like every other class.
func createStatisticsContract() ActionContract {
	return ActionContract{
		ActionType:      "create_statistics",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"CREATE privilege on the schema and table ownership (or MAINTAIN)",
		},
		Prechecks: []string{
			"correlated predicate evidence still exists",
			"statistics object does not already exist",
			"sample query is attached for verification",
		},
		Guardrails: []string{
			"pg_sage form only: " + extstats.NamePrefix + " name in the table's schema, " +
				"kinds ndistinct/dependencies/mcv, 2-8 plain columns of one table",
			"SHARE UPDATE EXCLUSIVE on the table (CREATE STATISTICS and ANALYZE)",
			"lock_timeout",
			"statement_timeout",
		},
		ExecutionPlan: []string{
			"CREATE STATISTICS schema." + extstats.NamePrefix + "... ON columns FROM schema.table",
			"ANALYZE schema.table",
			"both in one transaction: all or none",
		},
		SuccessCriteria: []string{
			"extended statistics object exists and is built",
			"planner row estimates improve without slowing the targeted queries",
		},
		PostChecks: []string{
			"verify pg_statistic_ext row",
			"compare EXPLAIN row estimates before and after the ANALYZE",
		},
		RollbackClass: "reversible",
		Cooldown:      "configured rollback cooldown",
		AuditFields:   []string{"table", "statistics_name", "kinds", "columns", "case_id"},
	}
}

// revertCreatedStatisticsContract is the inverse half of
// create_statistics: DROP STATISTICS IF EXISTS of the object pg_sage
// created (the definition stays in the action's own SQL).
func revertCreatedStatisticsContract() ActionContract {
	return ActionContract{
		ActionType:          "revert_created_statistics",
		BaseRiskTier:        "moderate",
		ProviderSupport:     portableActionProviders(),
		RequiredPermissions: []string{"statistics object ownership"},
		Prechecks:           []string{"the object is a " + extstats.NamePrefix + " object"},
		Guardrails: []string{"DROP STATISTICS IF EXISTS of one object, never CASCADE",
			"lock_timeout"},
		ExecutionPlan:   []string{"DROP STATISTICS IF EXISTS schema." + extstats.NamePrefix + "..."},
		SuccessCriteria: []string{"statistics object is absent"},
		PostChecks:      []string{"verify pg_statistic_ext no longer has the object"},
		RollbackClass:   "reversible",
		Cooldown:        "configured rollback cooldown",
		AuditFields:     []string{"statistics_name", "action_id"},
	}
}
