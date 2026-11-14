package executor

// Index and DDL action contracts; each takes its action type from
// contractRegistry (see contract_registry.go).

func createIndexConcurrentlyContract(actionType string) ActionContract {
	return ActionContract{
		ActionType:      actionType,
		BaseRiskTier:    "moderate",
		ProviderSupport: lakebaseOwnedActionProviders(),
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
	}
}

func dropUnusedIndexContract(actionType string) ActionContract {
	return ActionContract{
		ActionType:      actionType,
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
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
			"earned autonomy: autonomous trust, tier3_moderate, 31-day ramp",
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
	}
}

func alterTableContract(actionType string) ActionContract {
	return ActionContract{
		ActionType:      actionType,
		BaseRiskTier:    "high",
		ProviderSupport: portableActionProviders(),
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
	}
}

func ddlPreflightContract(actionType string) ActionContract {
	return ActionContract{
		ActionType:      actionType,
		BaseRiskTier:    "high",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"read catalog statistics",
			"review migration output in PR or CI",
		},
		Prechecks: []string{
			"classify DDL lock level and rewrite behavior",
			"check live table size and activity evidence",
			"check pending locks and replica lag evidence",
		},
		Guardrails: []string{
			"direct execution disabled",
			"generate PR or migration script",
			"manual review required",
			"maintenance-window recommendation",
		},
		ExecutionPlan: []string{
			"generate migration SQL, rollback or mitigation plan, and verification SQL",
		},
		SuccessCriteria: []string{
			"reviewable migration artifact is produced",
			"verification SQL is attached to the originating case",
		},
		PostChecks: []string{
			"run verification SQL in CI or staging",
			"rerun migration safety analyzer after deployment",
		},
		RollbackClass: "forward_fix_only",
		Cooldown:      "none",
		AuditFields:   []string{"table", "ddl", "case_id", "risk_score"},
	}
}
