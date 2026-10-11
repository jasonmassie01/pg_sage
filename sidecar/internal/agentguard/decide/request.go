// Package decide is agent governance's gate composition (spec
// §6.2): the D-steps D1-D10 every agent-originated request passes, the
// mapping of existing MCP tools to capability classes (§6.2.6) and the
// cross-database re-check that keeps a principal active until its write
// commits (§6.2.7). Decider implements policy.AgentDecider through
// Decider.Policy; agent_query and the other agent_* tools call
// Decider.Decide directly.
package decide

import (
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Capability is an agent capability class (§5.2).
type Capability string

// Capability classes.
const (
	CapRead           Capability = "read"
	CapWriteInsert    Capability = "write_insert"
	CapWriteUpdate    Capability = "write_update"
	CapWriteDelete    Capability = "write_delete"
	CapDDLAdditive    Capability = "ddl_additive"
	CapDDLLocking     Capability = "ddl_locking"
	CapDDLDestructive Capability = "ddl_destructive"
	CapMaint          Capability = "maint"
	CapSandbox        Capability = "sandbox"
	CapPolicyProposal Capability = "policy_proposal"
)

// Valid reports whether c is one of the ten classes.
func (c Capability) Valid() bool { return capabilityRank[c] }

var capabilityRank = map[Capability]bool{CapRead: true, CapWriteInsert: true,
	CapWriteUpdate: true, CapWriteDelete: true, CapDDLAdditive: true, CapDDLLocking: true,
	CapDDLDestructive: true, CapMaint: true, CapSandbox: true, CapPolicyProposal: true}

// Writes reports a data write class (D7 needs PITR for these in prod).
func (c Capability) Writes() bool {
	return c == CapWriteInsert || c == CapWriteUpdate || c == CapWriteDelete
}

// DDL reports a schema change class.
func (c Capability) DDL() bool {
	return c == CapDDLAdditive || c == CapDDLLocking || c == CapDDLDestructive
}

// Mutates reports a class the change freeze (D6) stops.
func (c Capability) Mutates() bool { return c.Writes() || c.DDL() || c == CapMaint }

// Request is one agent-originated request as governance sees it.
type Request struct {
	// PrincipalID is the principal; "" is an agent with no principal (the
	// stdio client without mcp.stdio_principal), treated as unsponsored.
	PrincipalID string
	// Tool is the MCP tool; Kind and Capability derive from it when unset.
	Tool       string
	Kind       agentguard.ToolKind
	Capability Capability
	// Database is the fleet database name the request acts on; "" is a
	// request with no database (a policy proposal), for which D3 checks
	// nothing environment-specific.
	Database string
	// Objects are the relations and columns a brokered request touches
	// (D5); nil touches none.
	Objects []Object
	// GrantID is the grant a brokered request uses (D10); "" uses none.
	GrantID string
	// Narrowing marks a narrowing contract (§6.2.4).
	Narrowing bool
	// OperatorApproved marks a request a person approved: D8 and D9 no
	// longer bind (§6.2.2).
	OperatorApproved bool
	// TaskID is the trusted task claim, if any.
	TaskID string
}

// Object is one relation or column a request touches.
type Object struct {
	Schema   string
	Relation string
	// Column is "" for the relation as a whole.
	Column string
}

// Verdict is governance's decision on a Request.
type Verdict struct {
	// Allowed is false when a D-step failed; Reason names it.
	Allowed bool
	// MaxLevel caps the level (0-3) when Allowed.
	MaxLevel int
	// Park asks the caller to retry after RetryAfter (D9 agent_rate).
	Park       bool
	RetryAfter time.Duration
	Reason     agentguard.Reason
	// Step is the failing (or capping) step, "D1" … "D10".
	Step   string
	Detail string
	Fix    string
	// Env is the database's evaluated environment ("" without a database).
	Env envbind.Env
	// Capability is the class the request was decided as.
	Capability Capability
	// Principal is the principal as loaded for this decision.
	Principal agentguard.Principal
}

// Reason codes the D-steps add to the core's (§6.2.2, §8.1).
const (
	ReasonEnvCeiling     agentguard.Reason = "agent_env_ceiling"
	ReasonCapability     agentguard.Reason = "agent_capability"
	ReasonClassification agentguard.Reason = "agent_classification"
	ReasonChangeFreeze   agentguard.Reason = "agent_change_freeze"
	ReasonNoPITR         agentguard.Reason = "agent_no_pitr"
	ReasonRate           agentguard.Reason = "agent_rate"
	ReasonBudget         agentguard.Reason = "agent_budget"
	ReasonLeaseExpired   agentguard.Reason = "agent_lease_expired"
	ReasonUnavailable    agentguard.Reason = "agent_governance_unavailable"
)
