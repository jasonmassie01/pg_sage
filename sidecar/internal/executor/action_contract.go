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
