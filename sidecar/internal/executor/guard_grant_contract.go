package executor

// Agent grant contracts (AGENTDB-SPEC §6.3, §6.6). guard_grant widens: it
// runs operator-approved (L2) only. guard_revoke narrows (§6.2.4): it runs
// during an emergency stop and at every trust level, so a grant still
// expires on schedule (G1-08b).
const (
	ActionTypeGuardGrant  = "guard_grant"
	ActionTypeGuardRevoke = "guard_revoke"
)

// guardGrantContract grants an agent's broker role a column list on one
// object for a bounded time.
func guardGrantContract() ActionContract {
	return ActionContract{
		ActionType:      ActionTypeGuardGrant,
		BaseRiskTier:    "moderate",
		ProviderSupport: guardProviders(),
		RequiredPermissions: []string{"the privilege WITH GRANT OPTION, held by " +
			"pg_sage's own role (never through owner-role membership)"},
		Prechecks: []string{"principal active and sponsored", "operator approval",
			"environment within the principal's ceiling",
			"PUBLIC has no CREATE on the object's schema (P1)",
			"columns grantable in the environment (classification)"},
		Guardrails: []string{"one transaction", "lock_timeout 2s",
			"per-principal advisory lock", "column list, never a whole table"},
		ExecutionPlan: []string{"GRANT USAGE ON SCHEMA s TO sage_agentb_<id10>",
			"GRANT SELECT (cols) ON TABLE s.t TO sage_agentb_<id10>",
			"record grantor from aclexplode and expires_at on the database clock"},
		SuccessCriteria: []string{"the broker role holds exactly the listed columns"},
		PostChecks: []string{"aclexplode shows grantee, grantor and privileges " +
			"for every listed column"},
		RollbackClass: "reversible",
		Cooldown:      "none",
		AuditFields: []string{"principal_id", "database_id", "object", "columns",
			"grantor", "expires_at"},
	}
}

// guardRevokeContract revokes one registry grant GRANTED BY its recorded
// grantor and checks no residue is left.
func guardRevokeContract() ActionContract {
	return ActionContract{
		ActionType:          ActionTypeGuardRevoke,
		BaseRiskTier:        "safe",
		ProviderSupport:     guardProviders(),
		RequiredPermissions: []string{"membership in the recorded grantor"},
		Prechecks:           []string{"the grant is in the registry and not revoked"},
		Guardrails: []string{"one transaction", "lock_timeout 2s",
			"per-principal advisory lock", "leader-lease fence for the reconciler"},
		ExecutionPlan: []string{"REVOKE SELECT (cols) ON TABLE s.t FROM " +
			"sage_agentb_<id10> GRANTED BY <grantor>",
			"REVOKE USAGE ON SCHEMA s with the schema's last grant"},
		SuccessCriteria: []string{"the role holds none of the grant's privileges"},
		PostChecks:      []string{"no residue for (grantee, grantor)"},
		RollbackClass:   "no_rollback_needed",
		Cooldown:        "none",
		AuditFields:     []string{"principal_id", "database_id", "grant_id", "cause"},
		Narrowing:       true,
	}
}
