package agentguard

// ToolKind is the class of MCP tool a principal calls (§6.2.6, §6.4).
type ToolKind string

// Tool kinds.
const (
	// ToolRead is an existing read tool (findings, explain, status).
	ToolRead ToolKind = "read"
	// ToolPropose is an existing propose tool (apply_migration,
	// request_change, optimize_query, …): capped at L2 until G3.
	ToolPropose ToolKind = "propose"
	// ToolAgent is an agent_* tool (agent_query, agent_request_capability,
	// …): the D-steps apply in full.
	ToolAgent ToolKind = "agent"
)

// Access is the core's verdict on a principal calling a tool kind, before
// the gate composition (ledger, profile, environment, grants) narrows it.
// MaxLevel caps the level; it is meaningful only when Allowed.
type Access struct {
	Allowed  bool
	MaxLevel int
	Reason   Reason
}

// ProposeCap is the level existing propose tools are capped at until G3.
const ProposeCap = 2

// ToolAccess decides how far p may go with a tool kind:
//   - retired: nothing (agent_retired);
//   - frozen: reads only; proposals and agent_* are agent_frozen (D1, G1-12);
//   - unsponsored: reads work, proposals queue at L2, agent_* are
//     agent_unsponsored (D2, §6.4 migrated tokens, G1-11);
//   - tainted: proposals and agent_* are capped at L2 (D8, G1-12);
//   - otherwise proposals are capped at L2 and agent_* at L3.
func ToolAccess(p Principal, kind ToolKind) Access {
	switch {
	case p.Retired() || !p.Status.Valid():
		return Access{Reason: ReasonRetired}
	case kind == ToolRead:
		return Access{Allowed: true, MaxLevel: 3}
	case kind != ToolPropose && kind != ToolAgent:
		return Access{Reason: ReasonLevel0}
	case p.Frozen():
		return Access{Reason: ReasonFrozen}
	case kind == ToolAgent && !p.Sponsored():
		return Access{Reason: ReasonUnsponsored}
	case kind == ToolPropose || p.Tainted:
		return Access{Allowed: true, MaxLevel: ProposeCap}
	default:
		return Access{Allowed: true, MaxLevel: 3}
	}
}
