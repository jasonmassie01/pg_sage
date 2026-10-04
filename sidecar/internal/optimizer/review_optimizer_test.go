package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// admitAll runs each candidate through admission on sampleTableContext,
// with what-if w (nil: HypoPG unavailable, so candidates are unverified).
func admitAll(t *testing.T, w whatIfValidator, recs []Recommendation) (
	[]Recommendation, int) {
	t.Helper()
	opt := New(nil, fnTestOptimizerConfig(), 160000, fnNoopLog)
	if w != nil {
		opt.whatIf = w
	}
	tc := sampleTableContext()
	tc.WriteRateKnown = true
	var accepted []Recommendation
	rejected := 0
	for _, rec := range recs {
		a := opt.Admit(context.Background(), rec, tc)
		if a.Outcome != AdmitAccepted {
			rejected++
			continue
		}
		accepted = append(accepted, a.Rec)
	}
	return accepted, rejected
}

func analyzeOne(t *testing.T, recs []Recommendation) ([]Recommendation, int) {
	t.Helper()
	return admitAll(t, nil, recs)
}

// G3-B05: the rollback must drop exactly the index the DDL creates, in
// the table's schema — never an LLM-chosen pre-existing index.
func TestAdmit_DropDDLTargetsCreatedIndex(t *testing.T) {
	rec := sampleRecommendation()
	rec.DDL = "CREATE INDEX CONCURRENTLY idx_orders_cust_created " +
		"ON public.orders (customer_id, created_at)"
	rec.DropDDL = "DROP INDEX CONCURRENTLY IF EXISTS orders_pkey"
	accepted, _ := analyzeOne(t, []Recommendation{rec})
	if len(accepted) != 1 {
		t.Fatalf("accepted = %d, want 1", len(accepted))
	}
	want := `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_cust_created"`
	if accepted[0].DropDDL != want {
		t.Errorf("DropDDL = %q, want %q", accepted[0].DropDDL, want)
	}
}

// G3-B05: a missing drop_ddl is synthesized, too.
func TestAdmit_DropDDLSynthesizedWhenMissing(t *testing.T) {
	rec := sampleRecommendation()
	rec.DropDDL = ""
	accepted, _ := analyzeOne(t, []Recommendation{rec})
	if len(accepted) != 1 {
		t.Fatalf("accepted = %d, want 1", len(accepted))
	}
	want := `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_status"`
	if accepted[0].DropDDL != want {
		t.Errorf("DropDDL = %q, want %q", accepted[0].DropDDL, want)
	}
}

// G3-B05/B20: DDL that is unnamed, targets another table, or is UNIQUE
// is rejected; rec.Table comes from the analyzed context, not the LLM.
func TestAdmit_RejectsUnboundDDL(t *testing.T) {
	cases := map[string]string{
		"unnamed":     "CREATE INDEX CONCURRENTLY ON public.orders (status)",
		"other table": "CREATE INDEX CONCURRENTLY idx_x ON public.customers (status)",
		"unique":      "CREATE UNIQUE INDEX CONCURRENTLY idx_u ON public.orders (status)",
		"drop":        "DROP INDEX CONCURRENTLY idx_orders_status",
	}
	for name, ddl := range cases {
		rec := sampleRecommendation()
		rec.DDL = ddl
		accepted, rejected := analyzeOne(t, []Recommendation{rec})
		if len(accepted) != 0 || rejected != 1 {
			t.Errorf("%s: accepted=%d rejected=%d, want 0/1", name, len(accepted), rejected)
		}
	}
	rec := sampleRecommendation()
	rec.Table = "public.some_other_table"
	accepted, _ := analyzeOne(t, []Recommendation{rec})
	if len(accepted) != 1 || accepted[0].Table != "public.orders" {
		t.Errorf("rec.Table = %+v, want public.orders from context", accepted)
	}
}

// C05/G2-B19/G3-B13: identity is the normalized index definition, so two
// same-table candidates differ and cosmetic DDL changes do not.
func TestRecommendation_FindingIdentity(t *testing.T) {
	a := sampleRecommendation()
	b := sampleRecommendation()
	b.DDL = "CREATE INDEX CONCURRENTLY idx_orders_created ON public.orders (created_at)"
	c := sampleRecommendation()
	c.DDL = "create index  concurrently IDX_renamed on public.orders USING btree ( STATUS )"
	accepted, _ := analyzeOne(t, []Recommendation{a, b, c})
	if len(accepted) != 3 {
		t.Fatalf("accepted = %d, want 3", len(accepted))
	}
	idA, idB, idC := accepted[0].FindingIdentifier(),
		accepted[1].FindingIdentifier(), accepted[2].FindingIdentifier()
	if idA == idB {
		t.Errorf("distinct indexes share identity %q", idA)
	}
	if idA != idC {
		t.Errorf("same index definition, different identity: %q vs %q", idA, idC)
	}
	if !strings.HasPrefix(idA, "public.orders|") {
		t.Errorf("identity %q does not lead with the table", idA)
	}
	for _, r := range accepted {
		if r.Category != "missing_index" {
			t.Errorf("category = %q, want fixed missing_index", r.Category)
		}
	}
}

type fakeWhatIf struct {
	available bool
	result    WhatIfResult
	err       error
}

func (f fakeWhatIf) IsAvailable(context.Context) bool { return f.available }

func (f fakeWhatIf) Validate(
	context.Context, Recommendation, []QueryInfo,
) (WhatIfResult, error) {
	return f.result, f.err
}

func analyzeWithWhatIf(t *testing.T, w whatIfValidator) ([]Recommendation, int) {
	t.Helper()
	return admitAll(t, w, []Recommendation{sampleRecommendation()})
}

// G3-B06: a HypoPG evaluation that measured every query and shows
// no/negative/too-small improvement rejects the recommendation.
func TestAdmit_HypoPGRejectionDropsRec(t *testing.T) {
	for _, imp := range []float64{0, -12.5, 3} {
		accepted, rejected := analyzeWithWhatIf(t, fakeWhatIf{available: true,
			result: WhatIfResult{Improvement: imp, SizeBytes: 8192, Measured: 2}})
		if len(accepted) != 0 || rejected != 1 {
			t.Errorf("improvement %.1f: accepted=%d rejected=%d, want 0/1",
				imp, len(accepted), rejected)
		}
	}
}

// Phase 0 item 7: an inconclusive what-if (unavailable, error, nothing
// measurable, a query that could not be planned) is "unverified": the
// recommendation is kept for approval but never marked validated.
// (Previously this test asserted the same outcome as "neutral"; the
// verdict is now explicit so the executor can require approval.)
func TestAdmit_HypoPGInconclusiveIsUnverified(t *testing.T) {
	for name, w := range map[string]fakeWhatIf{
		"no measurement": {available: true},
		"error":          {available: true, err: errors.New("boom")},
		"unavailable":    {},
		"partial failure": {available: true, result: WhatIfResult{Improvement: 80,
			SizeBytes: 8192, Measured: 1, Failed: 1}},
	} {
		accepted, _ := analyzeWithWhatIf(t, w)
		if len(accepted) != 1 {
			t.Errorf("%s: accepted = %d, want 1", name, len(accepted))
			continue
		}
		if accepted[0].Validated || accepted[0].WhatIf != WhatIfUnverified ||
			accepted[0].WhatIfReason == "" {
			t.Errorf("%s: verdict = %q (%q) validated=%t, want unverified with a reason",
				name, accepted[0].WhatIf, accepted[0].WhatIfReason, accepted[0].Validated)
		}
	}
	accepted, _ := analyzeWithWhatIf(t, fakeWhatIf{available: true,
		result: WhatIfResult{Improvement: 40, SizeBytes: 8192, Measured: 2}})
	if len(accepted) != 1 || !accepted[0].Validated || accepted[0].WhatIf != WhatIfVerified {
		t.Fatalf("validated rec missing: %+v", accepted)
	}
}

// G3-B23: the builder only marks write rate known when the table had
// any recorded activity.
func TestWriteRateKnown(t *testing.T) {
	if writeRateKnown(collector.TableStats{}) {
		t.Error("table with no recorded activity marked write rate known")
	}
	if !writeRateKnown(collector.TableStats{NTupIns: 1}) {
		t.Error("table with activity not marked write rate known")
	}
}

// G3-B24: a non-CREATE recommendation is never rated below high_risk
// from a model's self-rating.
func TestRiskTier_NonCreateIgnoresSelfRating(t *testing.T) {
	for _, rec := range []Recommendation{
		{DDL: "DROP INDEX CONCURRENTLY idx", ActionRisk: "safe"},
		{DDL: "REINDEX INDEX CONCURRENTLY idx", ActionRisk: "moderate"},
	} {
		if got := RiskTierForRecommendation(rec); got != RiskHigh {
			t.Errorf("RiskTier(%+v) = %q, want high_risk", rec, got)
		}
	}
}

// G3-D10: savings were computed as if mean time (ms) were cost units at
// 0.01 ms each — a 100x under-estimate.
func TestComputeQuerySavings_Milliseconds(t *testing.T) {
	got := ComputeQuerySavings(10, 5, 1000)
	if got != 5*time.Second {
		t.Errorf("savings = %v, want 5s (5ms x 1000 calls)", got)
	}
}
