package executor

// revertCreatedIndexContract covers dropping an index this executor itself
// created, after its identity (OID) has been re-verified. It is the inverse
// half of create_index_concurrently, not a drop of a user's index, so it
// carries no approval guardrail.
func revertCreatedIndexContract() ActionContract {
	return ActionContract{
		ActionType:          "revert_created_index",
		BaseRiskTier:        "moderate",
		ProviderSupport:     lakebaseOwnedActionProviders(),
		RequiredPermissions: []string{"index ownership or maintenance role"},
		Prechecks: []string{
			"index OID equals the OID recorded when pg_sage created it",
			"verification verdict requires revert",
		},
		Guardrails:      []string{"DROP INDEX CONCURRENTLY", "lock_timeout"},
		ExecutionPlan:   []string{"DROP INDEX CONCURRENTLY IF EXISTS created_index"},
		SuccessCriteria: []string{"created index is absent"},
		PostChecks:      []string{"verify index no longer exists"},
		RollbackClass:   "reversible",
		Cooldown:        "configured rollback cooldown",
		AuditFields:     []string{"index_name", "index_oid", "action_id"},
	}
}

// restoreDroppedIndexContract re-creates an index pg_sage dropped itself,
// from the definition recorded with the drop (the soft-drop restore). It
// is safe and needs no approval: it only rebuilds what existed, online, and
// is authorized only for SQL equal to that recorded definition
// (authorizeDroppedIndexRestore). It is supported wherever the drop is.
func restoreDroppedIndexContract() ActionContract {
	return ActionContract{
		ActionType:          ActionTypeRestoreDroppedIndex,
		BaseRiskTier:        "safe",
		ProviderSupport:     portableActionProviders(),
		RequiredPermissions: []string{"index ownership or maintenance role"},
		Prechecks: []string{
			"SQL equals the CONCURRENTLY form of the definition recorded with the drop",
			"the drop's targeted queries regressed or a hint names the index",
		},
		Guardrails:      []string{"CREATE INDEX CONCURRENTLY", "lock_timeout", "statement_timeout"},
		ExecutionPlan:   []string{"CREATE [UNIQUE] INDEX CONCURRENTLY recorded_definition"},
		SuccessCriteria: []string{"the dropped index exists again with its definition"},
		PostChecks:      []string{"verify pg_index.indisvalid and indisready"},
		RollbackClass:   "reversible",
		Cooldown:        "configured rollback cooldown",
		AuditFields:     []string{"index_name", "table", "action_id"},
	}
}
