package executor

// replaceIndexContract covers the replace action (roadmap 2.3). Its
// approval guardrail makes the gate queue it for a human whatever the
// trust level: two non-atomic steps with partial states run only with an
// operator's approval (the earned class index_replace is capped at L2).
func replaceIndexContract() ActionContract {
	return ActionContract{
		ActionType:      ActionTypeReplaceIndex,
		BaseRiskTier:    "moderate",
		ProviderSupport: portableActionProviders(),
		RequiredPermissions: []string{
			"schema CREATE privilege",
			"table and index ownership or maintenance role",
		},
		Prechecks: []string{
			"the new index subsumes the old index",
			"the old index backs no constraint and enforces no uniqueness",
			"every foreign key the old index supports stays supported",
			"the old index's OID and definition are the ones proposed",
			"no binding fact forbids DDL on the table",
		},
		Guardrails: []string{
			"approval_required",
			"CREATE INDEX CONCURRENTLY",
			"DROP INDEX CONCURRENTLY",
			"table change lease held across both steps",
			"lock_timeout",
			"statement_timeout",
		},
		ExecutionPlan: []string{
			"CREATE INDEX CONCURRENTLY new_index ...",
			"verify pg_index.indisvalid and indisready of new_index",
			"DROP INDEX CONCURRENTLY old_index (soft drop: definition kept)",
		},
		SuccessCriteria: []string{
			"the new index is valid and the old index is absent",
			"targeted queries improve and the old index's users do not regress",
		},
		PostChecks: []string{
			"verify the new index is valid and the old index is absent",
			"judge targeted queries and the old index's users over the verification window",
			"watch the old index's users over the drop window; re-create it on regression",
		},
		RollbackClass: "reversible",
		Cooldown:      "configured cascade cooldown",
		AuditFields: []string{"table", "new_index", "old_index", "old_index_oid",
			"old_definition", "case_id"},
	}
}
