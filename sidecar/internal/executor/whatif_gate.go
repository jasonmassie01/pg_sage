package executor

import (
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// deterministicIndexCategories are the finding categories whose CREATE
// INDEX comes from a deterministic rule, not from LLM advice, so there is
// no what-if claim to verify. Only the missing-FK-index rule
// (analyzer/rules_fk_index.go) qualifies. Every other producer of a
// background CREATE INDEX is gated: the LLM optimizer (missing_index, and
// covering_index / partial_index / composite_index or any other free-text
// label written before v1.8.0) and migration_safety, whose safe
// alternative may come from the LLM fallback (migration/llm_fallback.go).
var deterministicIndexCategories = map[string]bool{"missing_fk_index": true}

// requireApprovalWithoutWhatIf adds the approval-required guardrail to a
// background CREATE INDEX whose HypoPG what-if was not verified (Phase 0
// item 7): an index admitted without a complete what-if measurement may
// still be proposed, but only an operator may run it. A finding with no
// recorded verdict (written before verdicts existed) fails closed, and so
// does any category not known to be deterministic (lifeos 1.8.3: a legacy
// covering_index finding ran unattended).
func requireApprovalWithoutWhatIf(f analyzer.Finding, contract *policy.ActionContract) {
	if contract == nil || contract.ActionType != "create_index_concurrently" {
		return
	}
	if deterministicIndexCategories[f.Category] &&
		!optimizer.IsOptimizerFinding(f.Category, f.Detail) {
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
