package executor

// Query, hint and role-tuning action contracts (see contractRegistry).

func applyQueryHintContract() ActionContract {
	return ActionContract{
		ActionType:      "apply_query_hint",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"write access to hint_plan.hints",
			"pg_hint_plan installed and active",
		},
		Prechecks: []string{
			"query still appears in pg_stat_statements",
			"hint syntax validates",
			"hinted EXPLAIN changed the plan within tuner.hint_cost_tolerance_pct " +
				"(detail.hint_evidence.verdict changed)",
			"rollback SQL is available",
			"recent rejected hint cooldown has elapsed",
		},
		Guardrails: []string{
			"approval required",
			"revalidation required",
			"cooldown after rollback",
			"no broad planner enable/disable toggles",
		},
		ExecutionPlan: []string{"insert reviewed hint into hint_plan.hints"},
		SuccessCriteria: []string{
			"hint row exists",
			"query latency does not regress (post-apply verification)",
		},
		PostChecks: []string{
			"run hint revalidation loop",
			"monitor query latency and error rate",
		},
		RollbackClass: "reversible",
		Cooldown:      "configured hint rollback cooldown",
		AuditFields:   []string{"queryid", "hint_text", "case_id"},
	}
}

func investigateQueryPlanContract() ActionContract {
	return ActionContract{
		ActionType:      "investigate_query_plan",
		BaseRiskTier:    "safe",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"read access to query text and EXPLAIN",
		},
		Prechecks: []string{
			"query text or queryid evidence exists",
			"statement_timeout is configured",
		},
		Guardrails: []string{
			"read-only investigation",
			"statement_timeout",
			"no catalog changes",
		},
		ExecutionPlan: []string{"capture EXPLAIN (ANALYZE, BUFFERS, VERBOSE)"},
		SuccessCriteria: []string{
			"plan evidence attached",
			"follow-up remediation path classified",
		},
		PostChecks: []string{
			"review plan for missing index, stale stats, memory spill, or rewrite",
		},
		RollbackClass: "not_applicable",
		Cooldown:      "none",
		AuditFields:   []string{"queryid", "case_id"},
	}
}

func prepareQueryRewriteContract() ActionContract {
	return ActionContract{
		ActionType:      "prepare_query_rewrite",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"read query statistics and plans",
			"submit application query change through version control",
		},
		Prechecks: []string{
			"rewrite has deterministic rationale",
			"semantic equivalence can be tested in CI or staging",
			"baseline plan and latency evidence are attached",
		},
		Guardrails: []string{
			"direct database execution disabled",
			"generate PR or script",
			"approval required",
		},
		ExecutionPlan: []string{
			"generate query rewrite artifact and verification checklist",
		},
		SuccessCriteria: []string{
			"rewritten query returns equivalent rows",
			"plan cost, latency, or temp IO improves",
		},
		PostChecks: []string{
			"compare old and rewritten EXPLAIN plans",
			"monitor query latency and error rate after deployment",
		},
		RollbackClass: "application_rollback",
		Cooldown:      "query-scoped",
		AuditFields:   []string{"case_id", "database", "queryid"},
	}
}

func promoteRoleWorkMemContract() ActionContract {
	return ActionContract{
		ActionType:      "promote_role_work_mem",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"ALTER ROLE privilege or admin role",
		},
		Prechecks: []string{
			"multiple queries for same role need repeated work_mem hints",
			"recommended value is below configured maximum",
			"role-level memory blast radius is estimated",
		},
		Guardrails: []string{
			"approval required",
			"bounded work_mem value",
			"verification after one workload window",
		},
		ExecutionPlan: []string{"ALTER ROLE ... SET work_mem"},
		SuccessCriteria: []string{
			"representative queries stop spilling to temp",
			"database memory pressure remains acceptable",
		},
		PostChecks: []string{
			"verify pg_roles.rolconfig",
			"monitor temp blocks and memory pressure",
		},
		RollbackClass: "reversible",
		Cooldown:      "role-scoped",
		AuditFields:   []string{"case_id", "database", "role", "work_mem"},
	}
}

func retireQueryHintContract() ActionContract {
	return ActionContract{
		ActionType:      "retire_query_hint",
		BaseRiskTier:    "safe",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"write access to sage query hint metadata",
		},
		Prechecks: []string{
			"hint status is broken or superseded",
			"replacement action is absent or already generated",
		},
		Guardrails: []string{
			"metadata-only update",
			"query-scoped cooldown",
			"no user table writes",
		},
		ExecutionPlan: []string{"mark query hint retired"},
		SuccessCriteria: []string{
			"hint no longer appears in active hint inventory",
		},
		PostChecks: []string{
			"verify hint no longer appears in active hints",
			"rerun tuner revalidation",
		},
		RollbackClass: "reversible",
		Cooldown:      "query-scoped",
		AuditFields:   []string{"case_id", "database", "queryid", "hint_text"},
	}
}

func prepareParameterizedQueryContract() ActionContract {
	return ActionContract{
		ActionType:      "prepare_parameterized_query",
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"read query statistics and submit application PR",
		},
		Prechecks: []string{
			"literal-heavy query evidence is attached",
			"normalized query shape is available",
			"semantic equivalence can be tested",
		},
		Guardrails: []string{
			"direct database execution disabled",
			"generate PR or script",
			"approval required",
		},
		ExecutionPlan: []string{"generate application parameterization plan"},
		SuccessCriteria: []string{
			"queryid churn decreases",
			"latency and plan stability remain acceptable",
		},
		PostChecks: []string{
			"compare pg_stat_statements fingerprints",
			"monitor latency and errors after deployment",
		},
		RollbackClass: "application_rollback",
		Cooldown:      "query-scoped",
		AuditFields:   []string{"case_id", "database", "queryid"},
	}
}
