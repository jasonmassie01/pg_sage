package executor

// Incident and diagnostic action contracts (executor-30 split them out of
// action_contract.go; contractRegistry maps each type to its builder).

func incidentDiagnoseLockBlockersContract() ActionContract {
	return ActionContract{
		ActionType:      "diagnose_lock_blockers",
		BaseRiskTier:    "safe",
		ProviderSupport: portableActionProviders(),
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

func incidentDiagnoseRunawayQueryContract() ActionContract {
	return diagnosticContract(
		"diagnose_runaway_query",
		"read current query age, wait state, temp spill, and text",
		"confirm query is still active and matches incident evidence",
	)
}

func incidentDiagnoseConnectionExhaustionContract() ActionContract {
	return diagnosticContract(
		"diagnose_connection_exhaustion",
		"group pg_stat_activity by role, application, and state",
		"identify connection pressure source without changing sessions",
	)
}

func incidentDiagnoseWALReplicationContract() ActionContract {
	return diagnosticContract(
		"diagnose_wal_replication",
		"read replication lag and slot retention evidence",
		"identify WAL retention cause without dropping slots",
	)
}

func incidentDiagnoseStandbyConflictsContract() ActionContract {
	c := diagnosticContract(
		"diagnose_standby_conflicts",
		"read standby conflict counters by database",
		"identify replay conflict type without changing replica settings",
	)
	c.BaseRiskTier = "read_only"
	return c
}

func incidentSequenceCapacityMigrationContract() ActionContract {
	return ActionContract{
		ActionType:      "prepare_sequence_capacity_migration",
		BaseRiskTier:    "high",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"read sequence metadata",
			"submit reviewed migration through version control",
		},
		Prechecks: []string{
			"sequence still approaches max_value",
			"owning table and column are identified",
			"forward-fix migration is reviewed",
		},
		Guardrails: []string{
			"direct execution disabled",
			"generate PR or migration script",
			"manual review required",
		},
		ExecutionPlan: []string{
			"prepare capacity migration and verification SQL",
		},
		SuccessCriteria: []string{
			"sequence or owning column has sufficient runway",
		},
		PostChecks: []string{
			"verify sequence last_value and max_value",
			"confirm dependent column type has headroom",
		},
		RollbackClass: "forward_fix_only",
		Cooldown:      "none",
		AuditFields:   []string{"case_id", "database", "sequence"},
	}
}

func incidentCancelBackendContract() ActionContract {
	return ActionContract{
		ActionType:      "cancel_backend",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
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
		RollbackClass: ReversibilityMitigationOnly,
		Cooldown:      "incident-scoped",
		AuditFields:   []string{"case_id", "database", "pid", "query"},
	}
}

func incidentTerminateBackendContract() ActionContract {
	return ActionContract{
		ActionType:      "terminate_backend",
		BaseRiskTier:    "high",
		ProviderSupport: portableActionProviders(),
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

func diagnosticContract(actionType, execution, success string) ActionContract {
	return ActionContract{
		ActionType:      actionType,
		BaseRiskTier:    "safe",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"pg_monitor or pg_read_all_stats",
		},
		Prechecks: []string{
			"incident evidence is still open",
			"emergency stop is not active",
		},
		Guardrails: []string{
			"read-only diagnostic query",
			"statement_timeout",
			"no backend state changes",
		},
		ExecutionPlan:   []string{execution},
		SuccessCriteria: []string{success},
		PostChecks:      []string{"refresh incident evidence"},
		RollbackClass:   "not_applicable",
		Cooldown:        "none",
		AuditFields:     []string{"case_id", "database"},
	}
}
