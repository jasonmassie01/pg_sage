package tuning

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Look-alike priors (fleet learning) are evidence from other databases of
// the same fleet: they are shown, they may add caution, but they never
// raise confidence, remove an approval requirement or change trust.

type fakePriors struct {
	mu    sync.Mutex
	byCls map[string]*LookalikePrior
	err   error
	calls []priorCall
}

type priorCall struct {
	database, class string
	tables          []string
}

func (f *fakePriors) LookalikePrior(_ context.Context, database, class string,
	tables []string) (*LookalikePrior, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, priorCall{database, class, append([]string(nil), tables...)})
	if f.err != nil {
		return nil, f.err
	}
	return f.byCls[class], nil
}

func goodPrior() *LookalikePrior {
	return &LookalikePrior{Databases: 3, MinSimilarity: 0.8, Match: "table_shape",
		Improved: 9, Neutral: 1, Regressed: 0}
}

func badPrior() *LookalikePrior {
	return &LookalikePrior{Databases: 2, MinSimilarity: 0.7, Match: "action_class",
		Improved: 1, Neutral: 0, Regressed: 4}
}

func withPriors(t *testing.T, h *harness, p *fakePriors) {
	t.Helper()
	h.agent.deps.Priors = p
}

func TestLookalike_PriorIsShownLabelledAndPassedTheProposal(t *testing.T) {
	s := defaultSettings()
	s.DatabaseName = "tenant_7"
	h := newHarnessWith(t, s, indexAnswer(t))
	p := &fakePriors{byCls: map[string]*LookalikePrior{"index_create": goodPrior()}}
	withPriors(t, h, p)
	out := tune(t, h)
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok {
		t.Fatal("no index finding")
	}
	prior, ok := f.Detail[DetailLookalikePrior].(map[string]any)
	if !ok {
		t.Fatalf("detail = %v, want a look-alike prior", f.Detail)
	}
	if prior["source"] != LookalikeSource || prior["improved"] != 9 ||
		prior["outcomes"] != 10 || prior["databases"] != 3 {
		t.Fatalf("prior = %v", prior)
	}
	if v, _ := prior["wilson_low"].(float64); v <= 0 || v >= 0.9 {
		t.Fatalf("wilson_low = %v, want a lower bound below 9/10", prior["wilson_low"])
	}
	if len(p.calls) != 1 || p.calls[0].database != "tenant_7" ||
		p.calls[0].class != "index_create" || len(p.calls[0].tables) != 1 ||
		p.calls[0].tables[0] != "public.orders" {
		t.Fatalf("prior calls = %+v", p.calls)
	}
}

func TestLookalike_NeverRaisesConfidenceOrRemovesApproval(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.outcomes = improvedOutcomes("index_create", "hypopg", -40, 5, 2)
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"index_create": {Databases: 9, MinSimilarity: 0.99, Improved: 500}}})
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if f.Detail["confidence_score"] != 0.4 {
		t.Fatalf("confidence_score = %v, want the local 2/5 untouched",
			f.Detail["confidence_score"])
	}
	why, gated := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !gated || !strings.Contains(why, "2 of 5") {
		t.Fatalf("approval requirement removed by a prior: %v", f.Detail)
	}
}

func TestLookalike_UncalibratedGetsNoScoreFromThePrior(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"index_create": goodPrior()}})
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if _, has := f.Detail["confidence_score"]; has {
		t.Fatalf("a prior must not invent a local confidence score: %v", f.Detail)
	}
	if _, gated := f.Detail[analyzer.DetailApprovalRequired]; gated {
		t.Fatal("a good prior must not add an approval requirement")
	}
}

func TestLookalike_RegressionsOnLookAlikesRequireApproval(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.outcomes = improvedOutcomes("index_create", "hypopg", -40, 6, 6)
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"index_create": badPrior()}})
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	why, gated := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !gated || !strings.Contains(why, "look-alike") || !strings.Contains(why, "4 of 5") {
		t.Fatalf("look-alike regressions must require approval: %v", f.Detail)
	}
	if f.Detail["confidence_score"] != 1.0 {
		t.Fatal("the local confidence stays as measured")
	}
}

func TestLookalike_ExistingApprovalReasonIsKept(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.outcomes = improvedOutcomes("index_create", "hypopg", -40, 5, 1)
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"index_create": badPrior()}})
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	why, _ := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(why, "1 of 5") {
		t.Fatalf("the local reason was overwritten: %q", why)
	}
}

func TestLookalike_ErrorIsLoggedAndProposalUnchanged(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	withPriors(t, h, &fakePriors{err: errors.New("control database unreachable")})
	out := tune(t, h)
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok {
		t.Fatal("a prior failure must not drop the proposal")
	}
	if _, has := f.Detail[DetailLookalikePrior]; has {
		t.Fatal("no prior on error")
	}
	if !h.logs.contains("look-alike") || !h.logs.contains("control database unreachable") {
		t.Fatal("the prior failure must be logged with its cause")
	}
}

func TestLookalike_NilSourceAndNilPrior(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if _, has := f.Detail[DetailLookalikePrior]; has {
		t.Fatal("no source, no prior")
	}
	h = newHarness(t, indexAnswer(t))
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{}})
	out = tune(t, h)
	f, _ = findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if _, has := f.Detail[DetailLookalikePrior]; has {
		t.Fatal("a nil prior must not be recorded")
	}
}

func TestLookalike_EmptyPriorIsIgnored(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"index_create": {Databases: 2}}})
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if _, has := f.Detail[DetailLookalikePrior]; has {
		t.Fatal("a prior without outcomes is not evidence")
	}
}

func TestLookalike_OrdersOnlyWithinTheUncalibratedTier(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxProposalsPerCycle = 3
	h := newHarnessWith(t, s, answer(proposalsJSON(t,
		map[string]any{"type": "guc", "name": "work_mem", "value": "64MB",
			"evidence": []string{"S1"}, "expected_change_pct": -60},
		map[string]any{"type": "reloption", "table": "public.orders",
			"option": "autovacuum_vacuum_scale_factor", "value": "0.02",
			"evidence": []string{"T1"}, "expected_change_pct": -10},
		map[string]any{"type": "index_create",
			"ddl":      "CREATE INDEX CONCURRENTLY orders_c_idx ON public.orders (customer_id)",
			"evidence": []string{"S1"}, "expected_change_pct": -50})))
	h.store.outcomes = improvedOutcomes("index_create", "hypopg", -40, 5, 5)
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"reloption": goodPrior()}})
	out := tune(t, h)
	if len(out.Findings) != 3 {
		t.Fatalf("findings = %d, want 3", len(out.Findings))
	}
	if out.Findings[0].Category != optimizer.OptimizerCategory {
		t.Fatalf("first = %s: a locally calibrated proposal stays ahead of any prior",
			out.Findings[0].Category)
	}
	if out.Findings[1].Category == "memory_tuning" {
		t.Fatalf("second = %s: within the uncalibrated tier the proposal with "+
			"look-alike evidence goes first", out.Findings[1].Category)
	}
}

func TestLookalike_CautionedProposalDoesNotGainRank(t *testing.T) {
	s := defaultSettings()
	h := newHarnessWith(t, s, answer(proposalsJSON(t,
		map[string]any{"type": "guc", "name": "work_mem", "value": "64MB",
			"evidence": []string{"S1"}, "expected_change_pct": -10},
		map[string]any{"type": "reloption", "table": "public.orders",
			"option": "autovacuum_vacuum_scale_factor", "value": "0.02",
			"evidence": []string{"T1"}, "expected_change_pct": -60})))
	withPriors(t, h, &fakePriors{byCls: map[string]*LookalikePrior{
		"guc": badPrior()}})
	out := tune(t, h)
	if len(out.Findings) != 2 || out.Findings[0].Category == "memory_tuning" {
		t.Fatalf("a cautioned prior must not lift a proposal: %s first",
			out.Findings[0].Category)
	}
}

func TestLookalikePriorValueAndCaution(t *testing.T) {
	p := LookalikePrior{Improved: 3, Neutral: 1, Regressed: 1}
	if p.Outcomes() != 5 {
		t.Fatalf("outcomes = %d", p.Outcomes())
	}
	if p.Cautions() {
		t.Fatal("3 improved vs 1 regressed must not caution")
	}
	if !(LookalikePrior{Improved: 1, Regressed: 1}).Cautions() {
		t.Fatal("a tie cautions")
	}
	if (LookalikePrior{}).Cautions() || (LookalikePrior{}).Outcomes() != 0 {
		t.Fatal("an empty prior neither cautions nor counts")
	}
}
