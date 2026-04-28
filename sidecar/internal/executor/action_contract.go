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
	case "diagnose_lock_blockers":
		return incidentDiagnoseLockBlockersContract(), true
	case "cancel_backend":
		return incidentCancelBackendContract(), true
	case "terminate_backend":
		return incidentTerminateBackendContract(), true
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

func incidentDiagnoseLockBlockersContract() ActionContract {
	return ActionContract{
		ActionType:      "diagnose_lock_blockers",
		BaseRiskTier:    "safe",
		ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
		RequiredPermissions: []string{
			"pg_monitor or pg_read_all_stats",
		},
		Prechecks: []string{
			"incident evidence is still open",
			"emergency stop is not active",
		},
		Guardrails: []string{
			"read-only lock graph query",
			"statement_timeout",
			"no backend state changes",
		},
		ExecutionPlan: []string{"query pg_stat_activity and pg_locks"},
		SuccessCriteria: []string{
			"current blocker and blocked sessions are identified",
		},
		PostChecks:    []string{"refresh lock wait graph"},
		RollbackClass: "not_applicable",
		Cooldown:      "none",
		AuditFields:   []string{"case_id", "database", "blocker_pid"},
	}
}

func incidentCancelBackendContract() ActionContract {
	return ActionContract{
		ActionType:      "cancel_backend",
		BaseRiskTier:    "moderate",
		ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
		RequiredPermissions: []string{
			"pg_signal_backend or role membership allowing cancellation",
		},
		Prechecks: []string{
			"exact backend PID from current incident evidence",
			"PID still exists and still matches user/query/state evidence",
			"target is not a pg_sage backend",
		},
		Guardrails: []string{
			"approval required",
			"revalidate PID immediately before execution",
			"never target pg_sage backend",
			"prefer cancel before terminate",
		},
		ExecutionPlan: []string{"SELECT pg_cancel_backend(validated_pid)"},
		SuccessCriteria: []string{
			"blocked sessions no longer wait on the same backend",
		},
		PostChecks:    []string{"verify blocker PID no longer blocks waiters"},
		RollbackClass: "not_reversible",
		Cooldown:      "incident-scoped",
		AuditFields:   []string{"case_id", "database", "pid", "query"},
	}
}

func incidentTerminateBackendContract() ActionContract {
	return ActionContract{
		ActionType:      "terminate_backend",
		BaseRiskTier:    "high",
		ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
		RequiredPermissions: []string{
			"pg_signal_backend or role membership allowing termination",
		},
		Prechecks: []string{
			"exact backend PID from current incident evidence",
			"PID still exists and still matches user/query/state evidence",
			"cancel was attempted or judged insufficient",
			"target is not superuser, replication, autovacuum, or pg_sage",
		},
		Guardrails: []string{
			"approval required",
			"incident responder review",
			"revalidate PID immediately before execution",
			"never target superuser/replication/autovacuum/pg_sage backend",
			"prefer cancel first",
		},
		ExecutionPlan: []string{"SELECT pg_terminate_backend(validated_pid)"},
		SuccessCriteria: []string{
			"validated blocker backend is gone",
			"blocked workload is no longer waiting on the same blocker",
		},
		PostChecks: []string{
			"verify blocker PID is gone",
			"verify blocked sessions cleared or changed blockers",
		},
		RollbackClass: "not_reversible",
		Cooldown:      "incident-scoped",
		AuditFields:   []string{"case_id", "database", "pid", "query"},
	}
}
