package retention

// agentGateExemptions: approvals of agent policy proposals are the
// two-person and single-operator audit record (AGENTDB-SPEC §6.11, G1-14).
// A row goes only with its policy version (ON DELETE CASCADE); a pending
// single-operator review is never purged by age.
var agentGateExemptions = map[string]string{
	"policy_approvals": "approval audit of policy versions, deleted with its version " +
		"(ON DELETE CASCADE); pending post-hoc reviews must never age out",
}
