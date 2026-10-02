package runbook

import (
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// ProposalKind is what a runbook ends with.
type ProposalKind string

// Proposal kinds. None of them executes anything: an operator step is the
// graph node's manual step, an action is a typed action for the policy
// gate to judge when someone requests it, and an escalation hands over to
// a person.
const (
	ProposalOperatorStep ProposalKind = "operator_step"
	ProposalAction       ProposalKind = "action"
	ProposalEscalate     ProposalKind = "escalate"
)

// Proposal is a proposal node's content. Node is the graph node it
// addresses (operator steps and actions); the action's target comes from
// that hypothesis' evidence, never from the runbook.
type Proposal struct {
	Kind       ProposalKind `json:"kind"`
	Node       string       `json:"node,omitempty"`
	ActionType string       `json:"action_type,omitempty"`
}

// actionTypes are the executor action types an incident runbook may
// propose: incident diagnosis, backend signals and bounded table
// maintenance. Global configuration, retention deletes and index
// rebuilds are excluded through R2 (AI-SRE-SPEC §4). Each has an executor
// contract (checked by actions_contract_test.go).
var actionTypes = []string{
	"analyze_table", "cancel_backend", "create_statistics",
	"diagnose_connection_exhaustion", "diagnose_freeze_blockers", "diagnose_lock_blockers",
	"diagnose_runaway_query", "diagnose_standby_conflicts", "diagnose_vacuum_pressure",
	"diagnose_wal_replication", "investigate_query_plan",
	"prepare_sequence_capacity_migration", "terminate_backend", "vacuum_table",
}

// ActionTypes lists the action types a runbook may propose, sorted.
func ActionTypes() []string {
	out := append([]string(nil), actionTypes...)
	sort.Strings(out)
	return out
}

func knownAction(a string) bool { return containsString(actionTypes, a) }

func graphNode(id string) bool {
	_, ok := causal.NodeByID(causal.NodeID(id))
	return ok
}

func checkProposal(p *Proposal, path string, out *Problems) {
	if p == nil {
		out.add(CodeProposal, path, "a proposal node needs a proposal")
		return
	}
	switch p.Kind {
	case ProposalOperatorStep:
		if p.ActionType != "" {
			out.add(CodeProposal, path+".action_type", "an operator step takes no action")
		}
		checkProposalNode(p, path, out)
	case ProposalAction:
		if !knownAction(p.ActionType) {
			out.add(CodeUnknownAction, path+".action_type",
				"%q is not an action a runbook may propose", p.ActionType)
		}
		checkProposalNode(p, path, out)
	case ProposalEscalate:
		if p.Node != "" || p.ActionType != "" {
			out.add(CodeProposal, path, "an escalation takes no node or action")
		}
	default:
		out.add(CodeProposal, path+".kind", "unknown proposal kind %q", p.Kind)
	}
}

func checkProposalNode(p *Proposal, path string, out *Problems) {
	if p.Node == "" {
		out.add(CodeProposal, path+".node", "name the graph node this addresses")
		return
	}
	n, ok := causal.NodeByID(causal.NodeID(p.Node))
	switch {
	case !ok:
		out.add(CodeUnknownNode, path+".node", "%q is not a causal graph node", p.Node)
	case p.Kind == ProposalOperatorStep && n.OperatorStep == "":
		out.add(CodeProposal, path+".node", "%q has no operator step", p.Node)
	}
}

// Text is the proposal as an operator reads it.
func (p Proposal) Text() string {
	switch p.Kind {
	case ProposalOperatorStep:
		n, _ := causal.NodeByID(causal.NodeID(p.Node))
		return n.OperatorStep
	case ProposalAction:
		return "Proposed action " + p.ActionType + " addressing " + p.Node +
			" (not executed: requesting it goes through the policy gate and approval)."
	case ProposalEscalate:
		return "No automated suggestion: escalate to a DBA with this evidence."
	}
	return ""
}

func secondsDuration(s int64) time.Duration { return time.Duration(s) * time.Second }
