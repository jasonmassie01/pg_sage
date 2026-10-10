package agentguard

// OutOfGatePath is one entry of the closed list of paths that change agent
// access without policy.Gate → Executor.Apply (AGENTDB-SPEC §6.2.5). The
// list is closed: everything else, the watchdog and the trash purge
// included, goes through the gate. A census test fails when code that
// changes agent roles, grants or sessions appears outside the gate and
// outside this list.
type OutOfGatePath string

// The closed list.
const (
	// OutOfGateKillFallback is the kill switch's direct fallback when the
	// control database or the gate is unreachable (§6.10). Audit: a local
	// append-only log, reconciled into sage.action_log when the control
	// database returns.
	OutOfGateKillFallback OutOfGatePath = "kill_direct_fallback"
	// OutOfGateManualRunbook is the manual SQL runbook in
	// docs/agent-guard.md, for when pg_sage is down. Audit: the operator's
	// own records; a startup self-check notes changed agent roles.
	OutOfGateManualRunbook OutOfGatePath = "manual_sql_runbook"
	// OutOfGateBreakGlass is the break-glass login (E1), for when the IdP is
	// down. Audit: auth_audit and an alert on every use.
	OutOfGateBreakGlass OutOfGatePath = "break_glass_login"
	// OutOfGateDecommissionAck is the AgentDB decommission acknowledgement
	// (§12). Audit: sage.agentdb_decommission, with actor and time.
	OutOfGateDecommissionAck OutOfGatePath = "decommission_acknowledgement"
)

// OutOfGatePaths returns the closed list in spec order.
func OutOfGatePaths() []OutOfGatePath {
	return []OutOfGatePath{OutOfGateKillFallback, OutOfGateManualRunbook,
		OutOfGateBreakGlass, OutOfGateDecommissionAck}
}

// IsOutOfGatePath reports whether p is on the closed list.
func IsOutOfGatePath(p OutOfGatePath) bool {
	for _, have := range OutOfGatePaths() {
		if have == p {
			return true
		}
	}
	return false
}
