package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schema"
)

// An open recommendation stored as unverified (HypoPG was missing, or an
// older release never evaluated it) is re-checked when it is reloaded, so
// installing HypoPG later lets it become verified (and act autonomously)
// or rejected (and resolved), instead of being re-emitted as unverified
// forever. A verified one is not re-evaluated every cycle.

type countingWhatIf struct {
	fakeWhatIf
	calls int
}

func (c *countingWhatIf) Validate(ctx context.Context, rec Recommendation,
	q []QueryInfo) (WhatIfResult, error) {
	c.calls++
	return c.fakeWhatIf.Validate(ctx, rec, q)
}

const reverifyDDL = `"ddl":"CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)"`

const unverifiedDetail = `{` + reverifyDDL +
	`,"what_if_verdict":"unverified","what_if_reason":"HypoPG unavailable",` +
	`"confidence_score":0.6,"estimated_improvement_pct":40,"queryids":[7,8]}`

const legacyUnvalidatedDetail = `{` + reverifyDDL +
	`,"hypopg_validated":false,"confidence_score":0.9}`

func reverifyOptimizer(w whatIfValidator) *Optimizer {
	return &Optimizer{cfg: fnTestOptimizerConfig(), logFn: noopLog2, whatIf: w}
}

func measured(improvement float64) WhatIfResult {
	return WhatIfResult{Measured: 2, Improvement: improvement, SizeBytes: 8192}
}

func TestReloadRecommendation_ReverifiesUnverifiedWhenHypoPGAppears(t *testing.T) {
	for name, detail := range map[string]string{
		"unverified": unverifiedDetail, "legacy": legacyUnvalidatedDetail} {
		w := &countingWhatIf{fakeWhatIf: fakeWhatIf{available: true, result: measured(72)}}
		rec, ok := reverifyOptimizer(w).reloadRecommendation(context.Background(), detail,
			sampleTableContext())
		if !ok {
			t.Fatalf("%s: a verified candidate must still be re-emitted", name)
		}
		if w.calls != 1 || rec.WhatIf != WhatIfVerified || !rec.Validated ||
			rec.WhatIfReason != "" {
			t.Fatalf("%s: calls=%d verdict=%q validated=%t reason=%q", name, w.calls,
				rec.WhatIf, rec.Validated, rec.WhatIfReason)
		}
		if rec.EstimatedImprovementPct != 72 || rec.CostEstimate == nil ||
			rec.CostEstimate.EstimatedSizeBytes != 8192 {
			t.Fatalf("%s: measured numbers not carried: %+v", name, rec)
		}
	}
}

func TestReloadRecommendation_KeepsStoredFieldsOnReverify(t *testing.T) {
	w := &countingWhatIf{fakeWhatIf: fakeWhatIf{available: true, result: measured(50)}}
	rec, ok := reverifyOptimizer(w).reloadRecommendation(context.Background(),
		unverifiedDetail, sampleTableContext())
	if !ok || len(rec.AffectedQueryIDs) != 2 || rec.AffectedQueryIDs[0] != 7 {
		t.Fatalf("queryids must survive re-verification: ok=%t %+v", ok, rec.AffectedQueryIDs)
	}
	if rec.Confidence < 0.6 {
		t.Fatalf("a verified reload must not lower the stored confidence: %.2f", rec.Confidence)
	}
}

func TestReloadRecommendation_RejectedOnReverifyIsNotReEmitted(t *testing.T) {
	w := &countingWhatIf{fakeWhatIf: fakeWhatIf{available: true, result: measured(2)}}
	rec, ok := reverifyOptimizer(w).reloadRecommendation(context.Background(),
		unverifiedDetail, sampleTableContext())
	if ok || w.calls != 1 || rec.WhatIf != WhatIfRejected {
		t.Fatalf("a measured non-gain must drop the candidate: ok=%t calls=%d verdict=%q",
			ok, w.calls, rec.WhatIf)
	}
}

func TestReloadRecommendation_StaysUnverifiedWithoutHypoPG(t *testing.T) {
	w := &countingWhatIf{fakeWhatIf: fakeWhatIf{available: false}}
	rec, ok := reverifyOptimizer(w).reloadRecommendation(context.Background(),
		unverifiedDetail, sampleTableContext())
	if !ok || w.calls != 0 || rec.WhatIf != WhatIfUnverified || rec.Validated {
		t.Fatalf("no HypoPG: ok=%t calls=%d verdict=%q", ok, w.calls, rec.WhatIf)
	}
	if !strings.Contains(rec.WhatIfReason, "unavailable") {
		t.Fatalf("reason must say why it is unverified: %q", rec.WhatIfReason)
	}
	rec, ok = reverifyOptimizer(nil).reloadRecommendation(context.Background(),
		unverifiedDetail, sampleTableContext())
	if !ok || rec.WhatIf != WhatIfUnverified {
		t.Fatalf("nil validator: ok=%t verdict=%q", ok, rec.WhatIf)
	}
}

func TestReloadRecommendation_EvaluationErrorStaysUnverified(t *testing.T) {
	w := &countingWhatIf{fakeWhatIf: fakeWhatIf{available: true,
		err: errors.New("connection refused")}}
	rec, ok := reverifyOptimizer(w).reloadRecommendation(context.Background(),
		unverifiedDetail, sampleTableContext())
	if !ok || w.calls != 1 || rec.WhatIf != WhatIfUnverified || rec.Validated {
		t.Fatalf("error: ok=%t calls=%d verdict=%q", ok, w.calls, rec.WhatIf)
	}
	if !strings.Contains(rec.WhatIfReason, "connection refused") {
		t.Fatalf("reason must carry the cause: %q", rec.WhatIfReason)
	}
}

func TestReloadRecommendation_VerifiedIsNotReevaluated(t *testing.T) {
	detail := `{` + reverifyDDL + `,"what_if_verdict":"verified","hypopg_validated":true}`
	w := &countingWhatIf{fakeWhatIf: fakeWhatIf{available: true, result: measured(1)}}
	rec, ok := reverifyOptimizer(w).reloadRecommendation(context.Background(), detail,
		sampleTableContext())
	if !ok || w.calls != 0 || rec.WhatIf != WhatIfVerified {
		t.Fatalf("verified reload: ok=%t calls=%d verdict=%q", ok, w.calls, rec.WhatIf)
	}
}

// Integration: an open finding stored unverified is verified by a real
// HypoPG session when the optimizer re-emits it, on a pool with a single
// connection (the findings rows must be released before the what-if
// session needs the connection).
func TestOpenRecommendations_ReverifiesWithRealHypoPG(t *testing.T) {
	pool := hypopgSessionPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, connectFindingsDB(t)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	ddl := "CREATE INDEX CONCURRENTLY idx_items_category ON hypopg_session_test.items (category)"
	insertOpenFinding(t, connectFindingsDB(t), "missing_index",
		"hypopg_session_test.items|btree(category)", map[string]any{
			"ddl": ddl, "table": "hypopg_session_test.items",
			"what_if_verdict": "unverified", "what_if_reason": "HypoPG unavailable",
			"confidence_score": 0.7,
		})
	o := New(nil, nil, pool, fnTestOptimizerConfig(), 170000, 8192,
		func(c, f string, a ...any) { t.Logf(c+": "+f, a...) })
	tc := TableContext{Schema: "hypopg_session_test", Table: "items", LiveTuples: 20000,
		Columns: []ColumnInfo{{Name: "id", Type: "integer", IsNullable: true},
			{Name: "category", Type: "integer", IsNullable: true}},
		Queries: []QueryInfo{{QueryID: 1, Calls: 1000, TotalTimeMs: 100000,
			Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}}
	recs, open := o.openRecommendations(ctx, tc)
	if ctx.Err() != nil {
		t.Fatal("re-verification waited for a connection held by the findings query")
	}
	if !open || len(recs) != 1 {
		t.Fatalf("open=%t recs=%d", open, len(recs))
	}
	if recs[0].WhatIf != WhatIfVerified || !recs[0].Validated ||
		recs[0].EstimatedImprovementPct < 50 {
		t.Fatalf("want verified with a measured gain, got %q (%q) %.1f%%",
			recs[0].WhatIf, recs[0].WhatIfReason, recs[0].EstimatedImprovementPct)
	}
	assertHypoPGSessionClean(t, pool)
}
