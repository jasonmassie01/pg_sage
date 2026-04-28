package executor

import "errors"

type ActionContract struct {
	ActionType          string
	BaseRiskTier        string
	ProviderSupport     []string
	RequiredPermissions []string
	Prechecks           []string
	Guardrails          []string
	ExecutionPlan       []string
	SuccessCriteria     []string
	PostChecks          []string
	RollbackClass       string
	Cooldown            string
	AuditFields         []string
}

func (c ActionContract) Validate() error {
	if c.ActionType == "" || c.BaseRiskTier == "" {
		return errors.New("action contract missing type or risk tier")
	}
	if len(c.PostChecks) == 0 {
		return errors.New("action contract missing post-checks")
	}
	if c.RollbackClass == "" {
		return errors.New("action contract missing rollback class")
	}
	return nil
}

func AnalyzeTableContract() ActionContract {
	return ActionContract{
		ActionType:      "analyze_table",
		BaseRiskTier:    "safe",
		ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
		RequiredPermissions: []string{
			"table ownership or ANALYZE privilege",
		},
		Prechecks: []string{
			"table exists",
			"table is not excluded by policy",
			"emergency stop is not active",
			"stale-stat evidence exists",
			"cooldown window has elapsed",
		},
		Guardrails: []string{
			"dedicated connection",
			"statement_timeout",
			"per-table cooldown",
			"fleet analyze semaphore",
			"per-cluster safe-action concurrency limit",
		},
		ExecutionPlan: []string{"ANALYZE qualified_table"},
		SuccessCriteria: []string{
			"last_analyze advances or analyze_count increases",
			"stale-stat case no longer fires",
		},
		PostChecks: []string{
			"verify last_analyze or analyze_count changed",
			"rerun analyzer",
			"compare planner row-estimate error where available",
		},
		RollbackClass: "no_rollback_needed",
		Cooldown:      "configured analyze cooldown",
		AuditFields: []string{
			"table",
			"prior_last_analyze",
			"new_last_analyze",
			"prior_estimate_error",
			"post_action_estimate_error",
			"case_id",
		},
	}
}

func ContractForActionType(actionType string) (ActionContract, bool) {
	switch actionType {
	case "analyze_table":
		return AnalyzeTableContract(), true
	case "create_index_concurrently":
		return ActionContract{
			ActionType:      actionType,
			BaseRiskTier:    "moderate",
			ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
			RequiredPermissions: []string{
				"schema CREATE privilege",
				"table ownership or maintenance role",
			},
			Prechecks: []string{
				"table exists",
				"index does not already cover target columns",
				"disk pressure is below safety threshold",
			},
			Guardrails: []string{
				"CREATE INDEX CONCURRENTLY",
				"lock_timeout",
				"statement_timeout",
				"maintenance-window enforcement",
			},
			ExecutionPlan: []string{"CREATE INDEX CONCURRENTLY ..."},
			SuccessCriteria: []string{
				"index is valid and ready",
				"planner can choose the index where applicable",
			},
			PostChecks:    []string{"verify pg_index.indisvalid and indisready"},
			RollbackClass: "reversible",
			Cooldown:      "configured cascade cooldown",
			AuditFields:   []string{"table", "columns", "index_name", "case_id"},
		}, true
	case "drop_unused_index":
		return ActionContract{
			ActionType:      actionType,
			BaseRiskTier:    "moderate",
			ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
			RequiredPermissions: []string{
				"index ownership or maintenance role",
			},
			Prechecks: []string{
				"index exists",
				"unused window has elapsed",
				"index is not protected by policy",
			},
			Guardrails: []string{
				"DROP INDEX CONCURRENTLY",
				"approval required",
				"maintenance-window enforcement",
			},
			ExecutionPlan: []string{"DROP INDEX CONCURRENTLY ..."},
			SuccessCriteria: []string{
				"index is absent",
				"no protected dependency was removed",
			},
			PostChecks:    []string{"verify index no longer exists"},
			RollbackClass: "reversible",
			Cooldown:      "configured cascade cooldown",
			AuditFields:   []string{"index_name", "table", "case_id"},
		}, true
	case "alter_table":
		return ActionContract{
			ActionType:      actionType,
			BaseRiskTier:    "high",
			ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
			RequiredPermissions: []string{
				"table ownership or maintenance role",
			},
			Prechecks: []string{
				"table exists",
				"DDL has a reviewed forward-fix plan",
				"maintenance window is active",
			},
			Guardrails: []string{
				"approval required",
				"lock_timeout",
				"statement_timeout",
				"maintenance-window enforcement",
			},
			ExecutionPlan:   []string{"ALTER TABLE ..."},
			SuccessCriteria: []string{"schema change is visible in catalog"},
			PostChecks:      []string{"verify expected schema state"},
			RollbackClass:   "forward_fix_only",
			Cooldown:        "configured cascade cooldown",
			AuditFields:     []string{"table", "ddl", "case_id"},
		}, true
	default:
		return ActionContract{}, false
	}
}
