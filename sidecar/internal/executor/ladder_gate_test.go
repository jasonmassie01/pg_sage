package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-03a: an action class that is not cheap to revert runs unattended
// only after a passed tier-2 (clone) test; without one it is queued for a
// person at every trust level.

const ladderDropSQL = `DROP INDEX CONCURRENTLY "public"."idx_ladder_a"`

func ladderDropFinding(detail map[string]any) analyzer.Finding {
	if detail == nil {
		detail = map[string]any{}
	}
	return analyzer.Finding{Category: "unused_index", ObjectType: "index",
		ObjectIdentifier: "public.idx_ladder_a", RecommendedSQL: ladderDropSQL,
		RollbackSQL: "CREATE INDEX CONCURRENTLY idx_ladder_a ON public.t (a)",
		ActionRisk:  "moderate", Detail: detail}
}

// ladderDetail is the finding-detail form of records a producer attached
// (a JSON round trip, as the durable finding stores it).
func ladderDetail(t *testing.T, recs ...verify.LadderRecord) []any {
	t.Helper()
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// rehearsedLadder is a finding-detail ladder with a passed clone
// rehearsal: fixtures that exercise what happens after an autonomous drop
// carry it, so the entry rule lets the drop run.
func rehearsedLadder() []any {
	return []any{map[string]any{"tier": float64(verify.TierClone), "name": "clone",
		"method": verify.MethodLadderRehearsal, "result": verify.LadderPassed,
		"at": "2026-11-16T09:00:00Z"}}
}

func passedClone() verify.LadderRecord {
	return verify.FromRehearsal(verify.Rehearsal{Passed: true, ReceiptID: 812,
		Provider: "neon"}, time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
}

func TestRevertCost_EveryContractClassIsClassified(t *testing.T) {
	for _, actionType := range ContractActionTypes() {
		if _, ok := revertCostByClass[actionType]; !ok {
			t.Errorf("action class %q has no revert-cost entry", actionType)
		}
	}
}

func TestRevertCost_Classes(t *testing.T) {
	cases := []struct {
		actionType, sql string
		cheap           bool
	}{
		{"create_index_concurrently", "CREATE INDEX CONCURRENTLY i ON t (a)", true},
		{"set_table_autovacuum",
			"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)", true},
		{"apply_query_hint", "INSERT INTO hint_plan.hints VALUES (1, '', 'x')", true},
		{"create_statistics", "CREATE STATISTICS s ON a, b FROM t", true},
		{"alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'", true},
		{"drop_unused_index", ladderDropSQL, false},
		{ActionTypeReplaceIndex, "", false},
		{"alter_table", "ALTER TABLE public.t ALTER COLUMN a TYPE bigint", false},
		{"alter_system_guc", "ALTER SYSTEM SET shared_buffers = '2GB'", false},
		{"", "", false},
		{"no_such_class", "SELECT 1", false},
	}
	for _, c := range cases {
		cheap, why := RevertCost(c.actionType, c.sql)
		if cheap != c.cheap {
			t.Errorf("%s %q: cheap = %t, want %t", c.actionType, c.sql, cheap, c.cheap)
		}
		if !cheap && strings.TrimSpace(why) == "" {
			t.Errorf("%s: a costly class gave no reason", c.actionType)
		}
	}
}

func TestLadderGate_DropWithoutCloneNeedsApproval(t *testing.T) {
	req := findingRequest(ladderDropFinding(nil), false)
	if !hasApprovalGuardrail(req) {
		t.Fatal("a drop with no tier-2 test carries no approval guardrail")
	}
	// A tier-1 pass is not enough for a costly class.
	tier1 := verify.FromDropMeasurement(verify.WhatIfEvidence{Verdict: "verified"},
		time.Now())
	req = findingRequest(ladderDropFinding(map[string]any{
		"ladder": ladderDetail(t, tier1)}), false)
	if !hasApprovalGuardrail(req) {
		t.Fatal("a drop with only a tier-1 pass ran without approval")
	}
}

func TestLadderGate_DropWithPassedCloneHasNoLadderGuardrail(t *testing.T) {
	req := findingRequest(ladderDropFinding(map[string]any{
		"ladder": ladderDetail(t, passedClone())}), false)
	if hasApprovalGuardrail(req) {
		t.Fatal("a drop rehearsed on a clone still needs approval")
	}
	failed := verify.FromRehearsal(verify.Rehearsal{Reason: "regressed"}, time.Now())
	req = findingRequest(ladderDropFinding(map[string]any{
		"ladder": ladderDetail(t, failed)}), false)
	if !hasApprovalGuardrail(req) {
		t.Fatal("a failed rehearsal counted as a tier-2 pass")
	}
}

// Error propagation: an unreadable ladder or what-if is no pass.
func TestLadderGate_UnreadableLadderIsNoPass(t *testing.T) {
	for _, v := range []any{"passed", map[string]any{"tier": 2}, []any{"x"}, 42} {
		f := ladderDropFinding(map[string]any{"ladder": v})
		if !hasApprovalGuardrail(findingRequest(f, false)) {
			t.Errorf("ladder %v gave a pass", v)
		}
		recs := findingLadder(f, "drop_unused_index", time.Now())
		if len(recs) == 0 || recs[len(recs)-1].Result != verify.LadderSkipped ||
			len(recs[len(recs)-1].Skipped) == 0 {
			t.Errorf("ladder %v: records %+v, want a skipped record with the error", v, recs)
		}
	}
}

func TestFindingLadder_WhatIfForACreate(t *testing.T) {
	f := optimizerFinding("verified")
	f.Detail["estimated_improvement_pct"] = 42.5
	f.Detail["what_if_measured"] = 3
	recs := findingLadder(f, "create_index_concurrently", time.Now())
	if len(recs) != 1 || recs[0].Tier != verify.TierPlanOnly || !recs[0].Passed() {
		t.Fatalf("records = %+v, want one tier-1 pass", recs)
	}
	if recs[0].Predicted == nil || *recs[0].Predicted != -42.5 {
		t.Fatalf("predicted = %v, want -42.5", recs[0].Predicted)
	}
	// A what-if verdict that is not a string is unreadable: skipped, with
	// the error text.
	f.Detail["what_if_verdict"] = 3.0
	recs = findingLadder(f, "create_index_concurrently", time.Now())
	if len(recs) != 1 || recs[0].Passed() || len(recs[0].Skipped) != 1 ||
		!strings.Contains(recs[0].Skipped[0].Reason, "float64") {
		t.Fatalf("records = %+v, want a skipped tier naming the bad type", recs)
	}
}

func TestFindingLadder_GinIsNotATierOnePass(t *testing.T) {
	f := optimizerFinding("verified")
	f.RecommendedSQL = "CREATE INDEX CONCURRENTLY idx_d ON public.docs USING gin (body)"
	recs := findingLadder(f, "create_index_concurrently", time.Now())
	if len(recs) != 1 || recs[0].Passed() || !strings.Contains(
		recs[0].Skipped[0].Reason, "gin") {
		t.Fatalf("records = %+v, want a skipped tier naming gin", recs)
	}
}

func TestFindingLadder_HintAndEmpty(t *testing.T) {
	f := analyzer.Finding{Category: "query_tuning", Detail: map[string]any{
		"hint_evidence": map[string]any{"verdict": "changed", "cost_delta_pct": -38.2}}}
	recs := findingLadder(f, "apply_query_hint", time.Now())
	if len(recs) != 1 || recs[0].Method != verify.MethodLadderHint || !recs[0].Passed() {
		t.Fatalf("hint records = %+v", recs)
	}
	if recs := findingLadder(analyzer.Finding{}, "analyze_table", time.Now()); len(recs) != 0 {
		t.Fatalf("empty finding records = %+v, want none", recs)
	}
}

// Boundary through the standing gate at full autonomy (L3): a drop
// without tier 2 is queued; with a passed tier 2 it executes.
func TestStandingGate_DropNeedsTierTwoAtL3(t *testing.T) {
	runtime := matrixRuntime("autonomous")
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtime, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return policy.UnattendedProfile(), nil
		},
		ValidateSQL: func(string) error { return nil },
		Now:         func() time.Time { return matrixNow },
	})
	rehearsed := gate.Authorize(context.Background(), findingRequest(ladderDropFinding(
		map[string]any{"ladder": ladderDetail(t, passedClone())}), false))
	if rehearsed.Verdict != policy.VerdictExecute {
		t.Fatalf("rehearsed drop = %+v, want execute", rehearsed)
	}
	plain := gate.Authorize(context.Background(),
		findingRequest(ladderDropFinding(nil), false))
	if plain.Verdict != policy.VerdictQueueApproval ||
		plain.Reason != policy.ReasonApprovalRequired {
		t.Fatalf("unrehearsed drop = %+v, want queue_approval", plain)
	}
	// A cheap class is unaffected: a verified index still executes.
	verified := gate.Authorize(context.Background(),
		findingRequest(optimizerFinding("verified"), false))
	if verified.Verdict != policy.VerdictExecute {
		t.Fatalf("verified index = %+v, want execute", verified)
	}
}

func TestQueueLadderFromContext(t *testing.T) {
	if got := queueLadderFrom(context.Background()); got != nil {
		t.Fatalf("empty context ladder = %+v", got)
	}
	ctx := withQueueLadder(context.Background(), []verify.LadderRecord{passedClone()})
	if got := queueLadderFrom(ctx); len(got) != 1 || got[0].Tier != verify.TierClone {
		t.Fatalf("context ladder = %+v", got)
	}
}

// The ladder reaches the action's prediction: the queue row's ladder when
// one was read, else the finding's.
func TestPredictionLadder(t *testing.T) {
	f := ladderDropFinding(map[string]any{"ladder": ladderDetail(t, passedClone())})
	got := actionLadder(context.Background(), f.Detail, ladderDropSQL, time.Now())
	if len(got) != 2 || got[0].Tier != verify.TierPlanOnly || got[0].Passed() ||
		got[1].Tier != verify.TierClone || !got[1].Passed() {
		t.Fatalf("finding ladder = %+v, want the missing what-if skipped, then the clone",
			got)
	}
	queued := []verify.LadderRecord{verify.FromDropMeasurement(
		verify.WhatIfEvidence{Verdict: "verified"}, time.Now()), passedClone()}
	ctx := withQueueLadder(context.Background(), queued)
	if got := actionLadder(ctx, f.Detail, ladderDropSQL, time.Now()); len(got) != 2 {
		t.Fatalf("queue ladder = %+v, want the two queued records", got)
	}
}

// An automatic rollback restores the prior state: the entry rule gates
// forward changes only, so the rollback of a created index (a drop) is
// not queued for want of a clone test.
func TestLadderGate_RollbackIsNotGated(t *testing.T) {
	f := ladderDropFinding(nil)
	if hasApprovalGuardrail(rollbackFindingRequest(f)) {
		t.Fatal("the rollback request of a drop carries the ladder's approval guardrail")
	}
	if !hasApprovalGuardrail(unboundFindingRequest(f, false)) {
		t.Fatal("the forward request of the same drop is not gated")
	}
	// The producer's own approval marker still applies to a rollback.
	marked := ladderDropFinding(map[string]any{
		analyzer.DetailApprovalRequired: "standby index usage unknown"})
	if !hasApprovalGuardrail(rollbackFindingRequest(marked)) {
		t.Fatal("the rollback lost the producer's approval marker")
	}
}
