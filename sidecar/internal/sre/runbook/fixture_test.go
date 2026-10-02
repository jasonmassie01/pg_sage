package runbook

import "math"

// Fixtures shared by the runbook tests: a valid lock runbook that reads
// long transactions, decides on the oldest transaction's age and proposes
// the idle-in-transaction operator step or an escalation.

var testVocab = Vocab{TriggerKinds: []string{"lock_blocking", "connection_pressure",
	"wal_retention", "plan_regression"}}

func num(v float64) *float64 { return &v }

func lockRunbook() Definition {
	return Definition{Name: "Idle-in-transaction blocker",
		Description: "Confirm an idle holder and point at its operator step.",
		Trigger: Trigger{Kinds: []string{"lock_blocking"},
			Nodes: []string{"idle_in_tx_holder"}},
		Start: "read_long_tx",
		Nodes: []Node{
			{ID: "read_long_tx", Type: NodeProbe, Probe: "long_transactions",
				Next: "old_tx"},
			{ID: "old_tx", Type: NodeDecision, When: &Predicate{Op: OpColumn,
				Probe: "long_transactions", Column: "xact_age_s", Agg: AggMax, Cmp: ">=",
				Value: num(300)}, Then: "end_tx", Else: "escalate"},
			{ID: "end_tx", Type: NodeProposal, Proposal: &Proposal{
				Kind: ProposalOperatorStep, Node: "idle_in_tx_holder"}},
			{ID: "escalate", Type: NodeProposal, Proposal: &Proposal{
				Kind: ProposalEscalate}},
		}}
}

// lockRunbookJSON is lockRunbook as an operator would write it.
const lockRunbookJSON = `{
  "name": "Idle-in-transaction blocker",
  "description": "Confirm an idle holder and point at its operator step.",
  "trigger": {"kinds": ["lock_blocking"], "nodes": ["idle_in_tx_holder"]},
  "start": "read_long_tx",
  "nodes": [
    {"id": "read_long_tx", "type": "probe", "probe": "long_transactions",
     "next": "old_tx"},
    {"id": "old_tx", "type": "decision",
     "when": {"op": "column", "probe": "long_transactions", "column": "xact_age_s",
              "agg": "max", "cmp": ">=", "value": 300},
     "then": "end_tx", "else": "escalate"},
    {"id": "end_tx", "type": "proposal",
     "proposal": {"kind": "operator_step", "node": "idle_in_tx_holder"}},
    {"id": "escalate", "type": "proposal", "proposal": {"kind": "escalate"}}
  ]
}`

var nan = math.NaN()
