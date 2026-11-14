package executor

// Vacuum, bloat and reindex action contracts (see contractRegistry).

func vacuumTableContract() ActionContract {
	return ActionContract{
		ActionType:      "vacuum_table",
		BaseRiskTier:    "safe",
		ProviderSupport: lakebaseOwnedActionProviders(),
		RequiredPermissions: []string{
			"table ownership or VACUUM privilege",
		},
		Prechecks: []string{
			"table exists",
			"dead tuple or bloat evidence still exceeds threshold",
			"IO pressure is below policy threshold",
		},
		Guardrails: []string{
			"dedicated connection",
			"statement_timeout",
			"per-table cooldown",
			"per-cluster safe-action concurrency limit",
		},
		ExecutionPlan: []string{"VACUUM qualified_table"},
		SuccessCriteria: []string{
			"last_vacuum advances or vacuum_count increases",
			"dead tuple ratio improves",
		},
		PostChecks: []string{
			"verify last_vacuum or vacuum_count changed",
			"rerun bloat analyzer",
		},
		RollbackClass: "no_rollback_needed",
		Cooldown:      "configured vacuum cooldown",
		AuditFields:   []string{"case_id", "database", "table"},
	}
}

func diagnoseFreezeBlockersContract() ActionContract {
	return diagnosticContract(
		"diagnose_freeze_blockers",
		"query database XID age and oldest backend_xmin holders",
		"freeze blockers and XID runway are identified",
	)
}

func setTableAutovacuumContract() ActionContract {
	return ActionContract{
		ActionType:      "set_table_autovacuum",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"table ownership or maintenance role",
		},
		Prechecks: []string{
			"table exists",
			"recommended reloptions are bounded by policy",
			"current reloptions captured for review",
		},
		Guardrails: []string{
			"earned autonomy: autonomous trust, tier3_moderate, 31-day ramp",
			"generate PR or migration script",
			"monitor post-change vacuum cadence",
		},
		ExecutionPlan: []string{"ALTER TABLE ... SET (autovacuum_*)"},
		SuccessCriteria: []string{
			"reloptions contain expected autovacuum settings",
			"future dead tuple ratio trends down",
		},
		PostChecks: []string{
			"verify pg_class.reloptions",
			"rerun vacuum tuning analyzer after one churn window",
		},
		// The finding ships ALTER TABLE ... RESET (...) as its rollback.
		RollbackClass: "reversible",
		Cooldown:      "configured cascade cooldown",
		AuditFields:   []string{"case_id", "database", "table", "reloptions"},
	}
}

func diagnoseVacuumPressureContract() ActionContract {
	return diagnosticContract(
		"diagnose_vacuum_pressure",
		"query table vacuum stats and oldest xmin holders",
		"vacuum blockers and maintenance pressure are identified",
	)
}

func planBloatRemediationContract() ActionContract {
	return ActionContract{
		ActionType:      "plan_bloat_remediation",
		BaseRiskTier:    "high",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"read relation size and bloat evidence",
			"submit online rebuild plan through change control",
		},
		Prechecks: []string{
			"bloat evidence still exceeds threshold",
			"maintenance window and disk headroom are reviewed",
			"online rebuild path is selected before execution",
		},
		Guardrails: []string{
			"direct execution disabled",
			"manual review required",
			"avoid VACUUM FULL without outage approval",
		},
		ExecutionPlan: []string{"generate online bloat remediation plan"},
		SuccessCriteria: []string{
			"table size or bloat ratio decreases",
			"application latency remains acceptable",
		},
		PostChecks: []string{
			"rerun bloat analyzer",
			"compare relation size and error rate",
		},
		RollbackClass: "forward_fix_only",
		Cooldown:      "table-scoped",
		AuditFields:   []string{"case_id", "database", "table"},
	}
}

func reindexConcurrentlyContract() ActionContract {
	return ActionContract{
		ActionType:      "reindex_concurrently",
		BaseRiskTier:    "moderate",
		ProviderSupport: lakebaseOwnedActionProviders(),
		RequiredPermissions: []string{
			"index ownership or maintenance role",
		},
		Prechecks: []string{
			"index exists",
			"disk headroom is sufficient for concurrent rebuild",
			"replica lag is acceptable",
		},
		Guardrails: []string{
			"REINDEX CONCURRENTLY",
			"approval required",
			"maintenance-window enforcement",
		},
		ExecutionPlan: []string{"REINDEX INDEX CONCURRENTLY ..."},
		SuccessCriteria: []string{
			"rebuilt index is valid",
			"index bloat or size decreases",
		},
		PostChecks: []string{
			"verify pg_index validity",
			"compare index size before and after",
		},
		RollbackClass: "reversible",
		Cooldown:      "index-scoped",
		AuditFields:   []string{"case_id", "database", "index"},
	}
}
