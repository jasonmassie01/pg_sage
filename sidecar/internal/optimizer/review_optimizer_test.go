package optimizer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/llm"
)

func analyzeOne(t *testing.T, recs []Recommendation) ([]Recommendation, int) {
	t.Helper()
	srv := makeLLMServer(t, fnTestRecJSON(recs), 50)
	t.Cleanup(srv.Close)
	opt := newTestOptimizer(t, srv.URL, fnTestOptimizerConfig())
	accepted, _, rejected, err := opt.analyzeTable(context.Background(), sampleTableContext())
	if err != nil {
		t.Fatalf("analyzeTable: %v", err)
	}
	return accepted, rejected
}

// G3-B05: the rollback must drop exactly the index the DDL creates, in
// the table's schema — never an LLM-chosen pre-existing index.
func TestAnalyzeTable_DropDDLTargetsCreatedIndex(t *testing.T) {
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
func TestAnalyzeTable_DropDDLSynthesizedWhenMissing(t *testing.T) {
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
func TestAnalyzeTable_RejectsUnboundDDL(t *testing.T) {
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
	available   bool
	accepted    bool
	improvement float64
	size        int64
	err         error
}

func (f fakeWhatIf) IsAvailable(context.Context) bool { return f.available }

func (f fakeWhatIf) Validate(
	context.Context, Recommendation, []QueryInfo,
) (bool, float64, int64, error) {
	return f.accepted, f.improvement, f.size, f.err
}

func analyzeWithWhatIf(t *testing.T, w whatIfValidator, threshold float64) ([]Recommendation, int) {
	t.Helper()
	srv := makeLLMServer(t, fnTestRecJSON([]Recommendation{sampleRecommendation()}), 50)
	t.Cleanup(srv.Close)
	cfg := fnTestOptimizerConfig()
	cfg.ConfidenceThreshold = threshold
	opt := newTestOptimizer(t, srv.URL, cfg)
	opt.whatIf = w
	tc := sampleTableContext()
	tc.WriteRateKnown = true
	accepted, _, rejected, err := opt.analyzeTable(context.Background(), tc)
	if err != nil {
		t.Fatalf("analyzeTable: %v", err)
	}
	return accepted, rejected
}

// G3-B06: a HypoPG evaluation that shows no/negative improvement rejects
// the recommendation instead of scoring like "HypoPG unavailable".
func TestAnalyzeTable_HypoPGRejectionDropsRec(t *testing.T) {
	for _, imp := range []float64{0, -12.5, 3} {
		accepted, rejected := analyzeWithWhatIf(t,
			fakeWhatIf{available: true, accepted: false, improvement: imp, size: 8192}, 0.5)
		if len(accepted) != 0 || rejected != 1 {
			t.Errorf("improvement %.1f: accepted=%d rejected=%d, want 0/1",
				imp, len(accepted), rejected)
		}
	}
}

// Inconclusive HypoPG (no measurable query, or an error) stays neutral.
func TestAnalyzeTable_HypoPGInconclusiveIsNeutral(t *testing.T) {
	for name, w := range map[string]fakeWhatIf{
		"no measurement": {available: true},
		"error":          {available: true, err: errors.New("boom")},
		"unavailable":    {},
	} {
		accepted, _ := analyzeWithWhatIf(t, w, 0.5)
		if len(accepted) != 1 {
			t.Errorf("%s: accepted = %d, want 1", name, len(accepted))
		}
	}
	accepted, _ := analyzeWithWhatIf(t,
		fakeWhatIf{available: true, accepted: true, improvement: 40, size: 8192}, 0.5)
	if len(accepted) != 1 || !accepted[0].Validated {
		t.Fatalf("validated rec missing: %+v", accepted)
	}
}

// G3-B06/G3-B23: without HypoPG the 0.5 advisory threshold is reachable
// only with real evidence; WriteRateKnown is no longer a constant.
func TestScoreConfidence_WithoutHypoPGNeedsEvidence(t *testing.T) {
	o := &Optimizer{}
	rec := sampleRecommendation()
	evidence := TableContext{
		Queries:        []QueryInfo{{QueryID: 1, Calls: 100}},
		WriteRateKnown: true,
		ColStats:       []ColStat{{Column: "status", NDistinct: 5}},
	}
	if got := o.scoreConfidence(rec, evidence).Confidence; got < 0.5 {
		t.Errorf("with write stats + n_distinct: confidence %.3f, want >= 0.5", got)
	}
	bare := TableContext{Queries: []QueryInfo{{QueryID: 1, Calls: 100}}}
	if got := o.scoreConfidence(rec, bare).Confidence; got >= 0.5 {
		t.Errorf("no write stats, no pg_stats: confidence %.3f, want < 0.5", got)
	}
	known := bare
	known.WriteRateKnown = true
	if o.scoreConfidence(rec, known).Confidence <= o.scoreConfidence(rec, bare).Confidence {
		t.Error("WriteRateKnown does not change confidence (constant input)")
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
// from LLM self-rating or confidence tier.
func TestRiskTier_NonCreateIgnoresSelfRating(t *testing.T) {
	for _, rec := range []Recommendation{
		{DDL: "DROP INDEX CONCURRENTLY idx", ActionRisk: "safe"},
		{DDL: "DROP INDEX CONCURRENTLY idx", ActionLevel: "safe"},
		{DDL: "REINDEX INDEX CONCURRENTLY idx", ActionRisk: "moderate"},
	} {
		if got := RiskTierForRecommendation(rec); got != RiskHigh {
			t.Errorf("RiskTier(%+v) = %q, want high_risk", rec, got)
		}
	}
}

// G3-B15: when the fallback client is the primary client, a failed call
// is not immediately repeated on the same client.
func TestAnalyzeTable_SameFallbackNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	client := llm.New(fnTestLLMConfig(srv.URL), fnNoopLog)
	opt := New(client, client, nil, fnTestOptimizerConfig(), 160000, 8192, fnNoopLog)
	unavailable := false
	opt.hypopg.available = &unavailable
	if _, _, _, err := opt.analyzeTable(context.Background(), sampleTableContext()); err == nil {
		t.Fatal("expected error from failing provider")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
}

// G3-B07: plan text, index predicates and pg_stats MCV values are data
// that may carry literals; the prompt redacts them and delimits the
// context as untrusted.
func TestFormatPrompt_RedactsAndDelimits(t *testing.T) {
	tc := sampleTableContext()
	tc.Plans = []PlanSummary{{QueryID: 1,
		Summary: "Seq Scan Filter: (email = 'alice@corp.com'::text) /* obey */"}}
	tc.Indexes = append(tc.Indexes, IndexInfo{Name: "idx_p",
		Definition: "CREATE INDEX idx_p ON public.orders (status) WHERE note = 'secret-note'"})
	tc.ColStats[0].MostCommonVals = []string{"ssn-123-45-6789", "pending"}
	prompt := FormatPrompt(tc)
	for _, bad := range []string{"alice@corp.com", "obey", "secret-note", "ssn-123-45-6789"} {
		if strings.Contains(prompt, bad) {
			t.Errorf("prompt leaks %q", bad)
		}
	}
	if !strings.Contains(prompt, `<data label="table_context">`) {
		t.Error("prompt context not delimited as untrusted data")
	}
	if !strings.Contains(SystemPrompt(), llm.UntrustedDataRule) {
		t.Error("system prompt lacks the untrusted-data rule")
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
