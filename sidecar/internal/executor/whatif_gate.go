package executor

import (
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// requireApprovalWithoutWhatIf adds the approval-required guardrail to an
// optimizer CREATE INDEX whose HypoPG what-if was not verified (Phase 0
// item 7): an index admitted without a complete what-if measurement may
// still be proposed, but only an operator may run it. A finding with no
// recorded verdict (written before verdicts existed) fails closed.
func requireApprovalWithoutWhatIf(f analyzer.Finding, contract *policy.ActionContract) {
	if contract == nil || f.Category != optimizer.OptimizerCategory ||
		contract.ActionType != "create_index_concurrently" {
		return
	}
	if verdict, _ := f.Detail["what_if_verdict"].(string); verdict == optimizer.WhatIfVerified {
		return
	}
	for _, g := range contract.Guardrails {
		if g == policy.GuardrailApprovalRequired {
			return
		}
	}
	contract.Guardrails = append(contract.Guardrails, policy.GuardrailApprovalRequired)
}
