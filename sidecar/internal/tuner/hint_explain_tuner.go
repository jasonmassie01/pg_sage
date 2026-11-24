package tuner

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// hintCompare is the configuration of the hinted EXPLAIN gate.
type hintCompare struct {
	pgMajor  int
	timeouts catalogread.Timeouts
}

// WithHintComparison turns on the hinted EXPLAIN gate (tuning-05): every
// hint the tuner or the tuning agent proposes is planned plain and hinted
// first (CompareHint), bounded by timeouts (the tuner's catalog timeouts),
// with tuner.hint_cost_tolerance_pct. pgVersionNum is server_version_num
// (0: read it). Set before use; the sidecar always sets it.
func (t *Tuner) WithHintComparison(pgVersionNum int, timeouts catalogread.Timeouts) *Tuner {
	t.compare = &hintCompare{pgMajor: pgVersionNum / 10000, timeouts: timeouts}
	return t
}

// HintComparisonEnabled reports whether hints go through the gate.
func (t *Tuner) HintComparisonEnabled() bool { return t != nil && t.compare != nil }

// HintedExplain validates and canonicalizes an agent hint like CheckHint,
// then compares it (the tuning agent's hinted_explain tool). Refusals of
// the comparison are a verdict, not an error: err is set only when the
// hint is not valid or the gate is off.
func (t *Tuner) HintedExplain(ctx context.Context, p HintProposal) (HintComparison, error) {
	if !t.HintComparisonEnabled() {
		return HintComparison{}, fmt.Errorf("%w: hinted EXPLAIN is not configured",
			ErrHintsUnavailable)
	}
	hint, err := t.checkHint(p)
	if err != nil {
		return HintComparison{}, err
	}
	cmp, _ := t.compareHint(ctx, p.QueryID, p.Query, hint, t.spillPlanFor(ctx, p, hint))
	return cmp, nil
}

// compareHint runs the gate for one statement and hint. ran is false when
// the gate is off. Without a connection, or with pg_hint_plan neither
// preloaded nor loadable into pg_sage's session, the verdict is
// unverifiable without a query.
func (t *Tuner) compareHint(ctx context.Context, queryID int64, query, hint string,
	spill []byte) (HintComparison, error) {
	tol := t.cfg.HintCostTolerancePct
	base := HintComparison{PGMajor: t.compare.pgMajor, TolerancePct: tol}
	avail := t.hints()
	switch {
	case t.pool == nil:
		return unverifiable(base, "no connection for the hinted EXPLAIN"), nil
	case avail == nil || !avail.SharedPreload && !avail.Preload && !avail.SessionLoad:
		return unverifiable(base, "pg_hint_plan is neither preloaded nor loadable into "+
			"pg_sage's session"), nil
	}
	cmp, err := CompareHint(ctx, t.pool, query, hint, t.compare.pgMajor, CompareOptions{
		Timeouts: t.compare.timeouts, TolerancePct: tol, SessionLoad: avail.SessionLoad,
		SpillPlan: spill})
	if err != nil {
		t.logFn("INFO", "tuner: hint %q for queryid=%d refused: %v", hint, queryID, err)
	}
	return cmp, err
}

// spillPlanFor is the cached plan a Set(work_mem) agent hint is judged by
// (nil for any other hint: no plan read).
func (t *Tuner) spillPlanFor(ctx context.Context, p HintProposal, hint string) []byte {
	if _, workMem := hintDirectiveKinds(hint); !workMem {
		return nil
	}
	return t.readPlan(ctx, p.QueryID, p.Query).JSON
}

// gateFinding runs the gate on a hint finding: hint_evidence goes into its
// detail; a refusal is an error; unverifiable drops the executable SQL, so
// the finding stays advisory. With the gate off the finding is unchanged.
func (t *Tuner) gateFinding(ctx context.Context, f analyzer.Finding, queryID int64,
	query, hint string, spill []byte) (analyzer.Finding, error) {
	if !t.HintComparisonEnabled() {
		return f, nil
	}
	cmp, err := t.compareHint(ctx, queryID, query, hint, spill)
	return withHintEvidence(f, cmp, err)
}

func withHintEvidence(f analyzer.Finding, cmp HintComparison, err error,
) (analyzer.Finding, error) {
	if err != nil {
		return analyzer.Finding{}, err
	}
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail["hint_evidence"] = cmp.Evidence()
	if cmp.Verdict == HintUnverifiable {
		f.RecommendedSQL, f.RollbackSQL = "", ""
	}
	return f, nil
}

// gateCandidate runs the gate on a deterministic hint that would be
// executable; refused is true when an operator rejected the hint (read
// first, so a rejected hint is never planned) or the comparison refused it.
// A statement the comparison refused cools down, so it is not planned twice
// again every cycle.
func (t *Tuner) gateCandidate(ctx context.Context, c candidate, hint string,
) (*HintComparison, bool) {
	if t.rejectedHint(ctx, c.QueryID, hint) {
		return nil, true
	}
	if !t.HintComparisonEnabled() || strings.TrimSpace(hint) == "" || !t.HintsAvailable() {
		return nil, false
	}
	cmp, err := t.compareHint(ctx, c.QueryID, c.Query, hint, t.plans[c.QueryID].JSON)
	if err != nil {
		t.recordTuned(c.QueryID)
		return nil, true
	}
	return &cmp, false
}
