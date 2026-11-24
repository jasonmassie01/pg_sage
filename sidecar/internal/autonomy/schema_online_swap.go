package autonomy

import (
	"context"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// onlineSwapNote labels the online script: evidence for the operator,
// never a proposal pg_sage routes or runs (migration-09b).
const onlineSwapNote = "Online column swap: no table rewrite; each step names its lock. " +
	"Recommend-only: run it yourself, step by step, after review."

// swapEvidenceFunc returns the online swap evidence for an invariant, or
// nil when it has none.
type swapEvidenceFunc func(context.Context, schemaguard.Invariant) map[string]any

// onlineSwapRecorder records schema decisions like schemaDecisionRecorder
// and adds a type tightening's online column swap to the evidence.
type onlineSwapRecorder struct {
	ledger *ledger.Service
	swap   swapEvidenceFunc
}

func (r onlineSwapRecorder) Record(ctx context.Context, record schemaguard.DecisionRecord) error {
	evidence := schemaDecisionEvidence(record)
	if r.swap != nil {
		for key, value := range r.swap(ctx, record.Invariant) {
			if _, taken := evidence[key]; !taken {
				evidence[key] = value
			}
		}
	}
	return recordSchemaDecision(ctx, r.ledger, record, evidence)
}

// readerSwapEvidence plans online swaps through q's bounded catalog reads.
func readerSwapEvidence(q catalogread.Querier) swapEvidenceFunc {
	return func(ctx context.Context, inv schemaguard.Invariant) map[string]any {
		return planOnlineSwapEvidence(ctx, q, inv)
	}
}

// planOnlineSwapEvidence plans the online swap of a type tightening's
// column to bigint: the script, or the reason there is none (a refusal or
// a failed read). Other invariants, and a nil reader, return nil.
func planOnlineSwapEvidence(
	ctx context.Context, q catalogread.Querier, inv schemaguard.Invariant,
) map[string]any {
	if q == nil || inv.Kind != schemaguard.InvariantTypeTightening || inv.Subject == "" {
		return nil
	}
	facts, err := plan.ReadSwapFacts(ctx, q, inv.Schema, inv.Table, inv.Subject)
	var swap plan.SwapPlan
	if err == nil {
		swap, err = plan.PlanColumnSwap(ctx, facts, plan.SwapRequest{Schema: inv.Schema,
			Table: inv.Table, Column: inv.Subject, TargetType: "bigint"})
	}
	if err != nil {
		return map[string]any{"online_swap_refused": err.Error()}
	}
	return map[string]any{"online_swap_script": swap.Script(),
		"online_swap_note": onlineSwapNote}
}
