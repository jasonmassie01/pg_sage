package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Agent role contracts (AGENTDB-SPEC §6.3, §6.6). The agent governance
// package builds and runs them; their action types are typed internal
// actions (no caller-shaped SQL), their change class is agent_access, and
// in G1 they run only operator-approved (L2).
const (
	ActionTypeGuardRoleEnsure = "guard_role_ensure"
	ActionTypeGuardRoleRetire = "guard_role_retire"
)

// guardProviders are the providers every agent governance action supports
// (§6.3: self-managed, RDS/Aurora, Cloud SQL/AlloyDB, Azure, Supabase,
// Neon).
func guardProviders() []string {
	return []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb", "azure",
		"supabase", "neon"}
}

// guardRoleEnsureContract creates or re-asserts an agent's two cluster
// roles with exactly the attributes the spec requires.
func guardRoleEnsureContract() ActionContract {
	return ActionContract{
		ActionType:      ActionTypeGuardRoleEnsure,
		BaseRiskTier:    "moderate",
		ProviderSupport: guardProviders(),
		RequiredPermissions: []string{"CREATEROLE (PostgreSQL 16 or later), not superuser",
			"CONNECT grant option on each database"},
		Prechecks: []string{"PostgreSQL 16 or later", "principal is active",
			"pg_sage's own role passes the self-check", "operator approval"},
		Guardrails: []string{"one transaction", "lock_timeout 2s",
			"per-role advisory lock", "SCRAM verifier computed client-side"},
		ExecutionPlan: []string{"CREATE ROLE or ALTER ROLE sage_agentb_<id10> LOGIN ...",
			"CREATE ROLE or ALTER ROLE sage_agent_<id10> NOLOGIN ...",
			"ALTER ROLE ... IN DATABASE d SET <timeouts>", "GRANT CONNECT ON DATABASE d"},
		SuccessCriteria: []string{"both roles exist with the specified attributes"},
		PostChecks: []string{"attributes and settings equal the spec",
			"no membership in a dangerous role", "owns nothing (AP-02 is zero)"},
		RollbackClass: "reversible",
		Cooldown:      "none",
		AuditFields:   []string{"principal_id", "cluster_key", "login_role", "broker_role"},
	}
}

// guardRoleRetireContract drops an agent's cluster roles after revoking
// their privileges in every database of the cluster.
func guardRoleRetireContract() ActionContract {
	return ActionContract{
		ActionType:          ActionTypeGuardRoleRetire,
		BaseRiskTier:        "high",
		ProviderSupport:     guardProviders(),
		RequiredPermissions: []string{"ADMIN option on the agent roles"},
		Prechecks:           []string{"PostgreSQL 16 or later", "operator approval"},
		Guardrails: []string{"lock_timeout 2s", "per-role advisory lock",
			"REVOKE and DROP OWNED BY in every database first"},
		ExecutionPlan:   []string{"per database: DROP OWNED BY <role>", "DROP ROLE <role>"},
		SuccessCriteria: []string{"the roles are gone from the cluster"},
		PostChecks:      []string{"role absent in every database of the cluster"},
		RollbackClass:   "not_reversible",
		Cooldown:        "none",
		AuditFields:     []string{"principal_id", "cluster_key", "login_role", "broker_role"},
	}
}

// PolicyContractFor is the gate contract of a typed action, for packages
// that build their own policy requests (agent governance).
func PolicyContractFor(actionType string) (*policy.ActionContract, bool) {
	contract, ok := ContractForActionType(actionType)
	if !ok {
		return nil, false
	}
	return policyContract(contract), true
}

// AuthorizeTyped asks gate to authorize a typed request built outside the
// executor and maps the verdict as Apply expects: anything but execute is
// a *WithheldError. No gate fails closed. reauthorize marks Apply's second
// authorization, after its waits.
func AuthorizeTyped(ctx context.Context, gate policy.Gate, req policy.ActionRequest,
	reauthorize bool) (ActionPolicyDecision, error) {
	decision := ActionPolicyDecision{Decision: PolicyDecisionBlocked, RiskTier: "unknown",
		BlockedReason: reasonNoStandingPolicy}
	if gate != nil {
		decision = standingPolicyDecision(gate.Authorize(ctx, req))
	}
	if decision.Decision != PolicyDecisionExecute {
		return decision, &WithheldError{Decision: decision, Reauthorized: reauthorize}
	}
	return decision, nil
}
