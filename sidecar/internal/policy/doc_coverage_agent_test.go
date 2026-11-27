package policy

import (
	"reflect"
	"testing"
)

// An agent's request is decided by agent governance first
// (agentguard/decide, through GateConfig.Agents), and its reason is recorded
// in sage.decision like the gate's own (policy-09 review). Those reasons
// must be on docs/policy-gate.md too, or an operator who meets
// agent_no_pitr in the ledger cannot look it up. The decide package imports
// policy, so its constants are read from source, not imported.
//
// No concurrent access tests: the collector and the gap check are pure
// functions over files read once.

const (
	agentDecideDir = "../agentguard/decide"
	agentCoreDir   = "../agentguard"
	// The D-steps declare 9 reasons today; fewer means the collector lost
	// the qualified agentguard.Reason form.
	minAgentDecideReasons = 9
)

// agentAccessReasons are the core reasons agentguard.ToolAccess returns,
// which D1 and D2 record unchanged. The other core reasons
// (grantor_lacks_privilege, revoke_incomplete, public_create_on_schema)
// refuse grant and role changes and never reach the gate.
var agentAccessReasons = []string{
	"agent_frozen", "agent_level0", "agent_retired", "agent_unsponsored",
}

func TestDocCoveragePolicyGateNamesEveryAgentDeciderReason(t *testing.T) {
	decide, err := collectPolicyConsts(agentDecideDir)
	if err != nil {
		t.Fatalf("collect agent decider constants: %v", err)
	}
	t.Logf("agent decider declares %d reasons", len(decide.Reasons))
	if len(decide.Reasons) < minAgentDecideReasons {
		t.Fatalf("collected %d agent decider reasons, want at least %d: %v",
			len(decide.Reasons), minAgentDecideReasons, decide.Reasons)
	}
	for _, want := range []string{"agent_no_pitr", "agent_rate", "agent_governance_unavailable"} {
		if !containsString(decide.Reasons, want) {
			t.Errorf("collector missed agent decider reason %q (got %v)", want, decide.Reasons)
		}
	}
	core, err := collectPolicyConsts(agentCoreDir)
	if err != nil {
		t.Fatalf("collect agentguard core constants: %v", err)
	}
	for _, reason := range agentAccessReasons {
		if !containsString(core.Reasons, reason) {
			t.Errorf("agentguard declares no reason %q; update agentAccessReasons", reason)
		}
	}
	doc := readDoc(t, policyGateDocPath)
	for _, reason := range undocumented(doc, append(decide.Reasons, agentAccessReasons...)) {
		t.Errorf("docs/policy-gate.md does not name agent reason `%s`", reason)
	}
}

// A constant typed with another package's Reason (agentguard.Reason in
// decide) is collected like a local one.
func TestDocCoverageFixtureCollectsQualifiedType(t *testing.T) {
	dir := writeFixture(t, map[string]string{"qualified.go": "package fixture\n\n" +
		"import \"example.com/agentguard\"\n\nconst (\n" +
		"\tReasonQualified agentguard.Reason = \"qualified\"\n" +
		"\tVerdictQualified agentguard.Verdict = \"qualified_verdict\"\n" +
		"\tOtherQualified agentguard.Other = \"not_a_reason\"\n)\n"})
	consts, err := collectPolicyConsts(dir)
	if err != nil {
		t.Fatalf("collect fixture constants: %v", err)
	}
	if !reflect.DeepEqual(consts.Reasons, []string{"qualified"}) {
		t.Errorf("reasons = %v, want [qualified]", consts.Reasons)
	}
	if !reflect.DeepEqual(consts.Verdicts, []string{"qualified_verdict"}) {
		t.Errorf("verdicts = %v, want [qualified_verdict]", consts.Verdicts)
	}
}
