package executor

// Freeze, kill switch and unfreeze contracts (spec §6.3, §6.10).
// guard_freeze and guard_kill only take access away: they are Narrowing,
// so the gate lets them through the emergency stop, a disabled executor,
// any trust level, the standing document, budgets and windows (§6.2.4).
// guard_unfreeze widens and needs an operator approval like any change.
const (
	ActionTypeGuardFreeze   = "guard_freeze"
	ActionTypeGuardKill     = "guard_kill"
	ActionTypeGuardUnfreeze = "guard_unfreeze"
)

// guardContainContract is shared by guard_freeze (one principal) and
// guard_kill (the kill switch's per-cluster steps).
func guardContainContract(actionType, scope string) ActionContract {
	return ActionContract{
		ActionType:      actionType,
		BaseRiskTier:    "safe",
		ProviderSupport: guardProviders(),
		RequiredPermissions: []string{"ADMIN option on the agent roles",
			"pg_signal_backend (to end agent sessions pg_sage does not inherit)"},
		Prechecks: []string{"none: narrowing works under the emergency stop and at any " +
			"trust level"},
		Guardrails: []string{"lock_timeout 2s per ALTER ROLE", "3 attempts"},
		ExecutionPlan: []string{"record each role's prior attributes",
			"ALTER ROLE <role> NOLOGIN CONNECTION LIMIT 0 (" + scope + ")",
			"cancel in-flight agent applies", "terminate agent backends on the primary " +
				"and each configured replica", "cancel pending approvals (cancelled_kill)"},
		SuccessCriteria: []string{"no agent backend remains", "new agent logins fail"},
		PostChecks: []string{"no agent backend on the primary or any configured replica",
			"roles NOLOGIN; principal frozen (flag set)"},
		RollbackClass: "reversible",
		Cooldown:      "none",
		AuditFields:   []string{"principal_id", "cluster_key", "prior_attrs", "reason"},
		Narrowing:     true,
	}
}

// guardUnfreezeContract restores prior_attrs and rotates the broker
// credential; after a kill it needs two people (§6.11).
func guardUnfreezeContract() ActionContract {
	return ActionContract{
		ActionType:      ActionTypeGuardUnfreeze,
		BaseRiskTier:    "moderate",
		ProviderSupport: guardProviders(),
		RequiredPermissions: []string{"ADMIN option on the agent roles",
			"CREATEROLE (PostgreSQL 16 or later)"},
		Prechecks: []string{"principal frozen", "operator approval (two admins after a kill)",
			"encryption_key"},
		Guardrails: []string{"one transaction", "lock_timeout 2s", "per-role advisory lock",
			"SCRAM verifier computed client-side"},
		ExecutionPlan: []string{"ALTER ROLE <role> WITH <prior LOGIN> CONNECTION LIMIT <prior>",
			"ALTER ROLE <broker> PASSWORD '<new scram verifier>'",
			"store the new sealed credential", "principal active"},
		SuccessCriteria: []string{"roles have their prior attributes",
			"the old broker password fails"},
		PostChecks:    []string{"grants restored; credentials rotated"},
		RollbackClass: "reversible",
		Cooldown:      "none",
		AuditFields:   []string{"principal_id", "cluster_key", "approved_by", "reason"},
	}
}
