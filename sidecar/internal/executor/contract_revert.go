package executor

// revertCreatedIndexContract covers dropping an index this executor itself
// created, after its identity (OID) has been re-verified. It is the inverse
// half of create_index_concurrently, not a drop of a user's index, so it
// carries no approval guardrail.
func revertCreatedIndexContract() ActionContract {
	return ActionContract{
		ActionType:          "revert_created_index",
		BaseRiskTier:        "moderate",
		ProviderSupport:     portableActionProviders(),
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
