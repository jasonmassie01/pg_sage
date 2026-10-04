package tuning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Proposal validation: every proposal is checked deterministically before
// it becomes a finding. Index creates keep the optimizer's validator,
// rejection memory and HypoPG what-if gate.

func ordersCase() Case {
	return Case{ID: "top_statement:101", Kind: CaseTopStatement, Weight: 0.9,
		Tables: []string{"public.orders"},
		Statements: []CaseStatement{{QueryID: 101,
			Text: "SELECT * FROM public.orders WHERE customer_id = $1", Class: ClassApp,
			Calls: 600, TotalMs: 6000, MeanMs: 10, Share: 0.9, Windowed: true,
			TempBlksWritten: 4800}}}
}

func ordersEvidence() evidenceSet {
	return evidenceSet{
		"S1": {ID: "S1", Kind: "statement", Ref: "queryid:101", Text: "600 calls"},
		"T1": {ID: "T1", Kind: "table", Ref: "public.orders", Text: "1M rows"},
		"R1": {ID: "R1", Kind: "tool:explain", Ref: "queryid:101", Text: "Seq Scan"},
	}
}

// validationSnap is ordersPair's current snapshot with two more indexes
// on public.orders and a configuration snapshot.
func validationSnap() *collector.Snapshot {
	_, cur := ordersPair()
	cur.Indexes = append(cur.Indexes,
		index("public", "orders", "orders_customer_idx",
			"CREATE INDEX orders_customer_idx ON public.orders USING btree (customer_id)", 0),
		index("public", "orders", "orders_customer_status_idx",
			"CREATE INDEX orders_customer_status_idx ON public.orders USING btree "+
				"(customer_id, status)", 400),
		index("public", "orders", "orders_status_idx",
			"CREATE INDEX orders_status_idx ON public.orders USING btree (status)", 0),
	)
	uq := index("public", "orders", "orders_ref_key",
		"CREATE UNIQUE INDEX orders_ref_key ON public.orders USING btree (ref)", 0)
	uq.IsUnique = true
	cur.Indexes = append(cur.Indexes, uq)
	cur.System.RelationStatsEpoch = cur.CollectedAt.Add(-30 * 24 * time.Hour)
	cur.ConfigData = &collector.ConfigSnapshot{PGSettings: []collector.PGSetting{
		{Name: "work_mem", Setting: "4096", Unit: "kB", Source: "default", Context: "user"},
		{Name: "shared_buffers", Setting: "16384", Unit: "8kB", Source: "configuration file",
			Context: "postmaster"},
		{Name: "random_page_cost", Setting: "4", Source: "default", Context: "user"},
	}}
	return cur
}

func judgeOne(t *testing.T, h *harness, confirmed []facts.Fact, p Proposal) Judged {
	t.Helper()
	return judgeCase(t, h, confirmed, ordersCase(), p)
}

// judgeCase judges p for case c over ordersPair's interval (t0 to t0+5m).
func judgeCase(t *testing.T, h *harness, confirmed []facts.Fact, c Case, p Proposal) Judged {
	t.Helper()
	prev, _ := ordersPair()
	cur := validationSnap()
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, confirmed, t0), confirmed,
		h.store.rejected)
	v.prepare(context.Background(), prev)
	return v.judge(context.Background(), c, ordersEvidence(), p)
}

func createProposal() Proposal {
	return Proposal{Type: ProposeIndexCreate, Evidence: []string{"S1", "R1"},
		DDL: "CREATE INDEX CONCURRENTLY orders_customer_created_idx ON public.orders " +
			"(customer_id, created_at)",
		Rationale: "the statement filters on customer_id", ExpectedChangePct: pct(-30)}
}

func TestJudge_IndexCreateVerifiedByHypoPG(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, createProposal())
	if j.Verdict != VerdictAdmitted || j.Finding == nil {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	if f.Category != optimizer.OptimizerCategory || f.RecommendedSQL == "" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["what_if_verdict"] != optimizer.WhatIfVerified {
		t.Fatalf("the executor's what-if gate reads what_if_verdict: %v", f.Detail)
	}
	p := j.Prediction
	if p.Method != verify.MethodHypoPG || p.ExpectedChangePct == nil ||
		*p.ExpectedChangePct != -40 || p.Metric != verify.MetricMeanExecTime {
		t.Fatalf("HypoPG's measurement (40%%) replaces the model's estimate: %+v", p)
	}
	if p.Source != PredictionSource || len(p.TargetQueryIDs) != 1 ||
		p.TargetQueryIDs[0] != 101 || j.Class != verify.ClassIndexCreate {
		t.Fatalf("prediction = %+v class %q", p, j.Class)
	}
	if got := h.indexes.admittedDDL(); len(got) != 1 || !strings.Contains(got[0],
		"orders_customer_created_idx") {
		t.Fatalf("admission ran on the proposal's DDL: %v", got)
	}
}

func TestJudge_IndexCreateUnverifiedKeepsTheModelEstimate(t *testing.T) {
	h := newHarness(t)
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		rec.Table, rec.Category = "public.orders", optimizer.OptimizerCategory
		rec.WhatIf, rec.WhatIfReason = optimizer.WhatIfUnverified, "HypoPG unavailable"
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitAccepted}
	}
	j := judgeOne(t, h, nil, createProposal())
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("an unverified index is admitted; the executor gate makes it "+
			"approval-only: %+v", j)
	}
	if j.Prediction.Method != verify.MethodModel || *j.Prediction.ExpectedChangePct != -30 {
		t.Fatalf("prediction = %+v", j.Prediction)
	}
	if j.Finding.Detail["what_if_verdict"] != optimizer.WhatIfUnverified {
		t.Fatalf("detail = %v", j.Finding.Detail)
	}
}

func TestJudge_IndexCreateRejections(t *testing.T) {
	for _, tc := range []struct {
		outcome optimizer.AdmissionOutcome
		want    Reason
	}{
		{optimizer.AdmitInvalid, ReasonInvalid},
		{optimizer.AdmitMeasured, ReasonAlreadyMeasured},
		{optimizer.AdmitRejected, ReasonWhatIfRejected},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			h := newHarness(t)
			h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
				return optimizer.Admission{Rec: rec, Outcome: tc.outcome, Reason: "because"}
			}
			j := judgeOne(t, h, nil, createProposal())
			if j.Verdict != VerdictRejected || j.Reason != tc.want || j.Finding != nil {
				t.Fatalf("judged = %+v", j)
			}
			if !strings.Contains(j.Detail, "because") {
				t.Fatalf("detail %q must carry the admission's reason", j.Detail)
			}
		})
	}
}

func TestJudge_IndexCreateOutsideTheCase(t *testing.T) {
	h := newHarness(t)
	p := createProposal()
	p.DDL = "CREATE INDEX CONCURRENTLY x ON app.invoices (id)"
	j := judgeOne(t, h, nil, p)
	if j.Verdict != VerdictRejected || j.Reason != ReasonOutOfCase {
		t.Fatalf("judged = %+v", j)
	}
	p.DDL = "CREATE INDEX CONCURRENTLY x ON sage.findings (id)"
	if j := judgeOne(t, h, nil, p); j.Reason != ReasonOutOfCase {
		t.Fatalf("pg_sage's own tables: %+v", j)
	}
	p.DDL = "DROP TABLE public.orders"
	if j := judgeOne(t, h, nil, p); j.Reason != ReasonUnsupported {
		t.Fatalf("not an index create: %+v", j)
	}
	if len(h.indexes.admittedDDL()) != 0 {
		t.Fatal("nothing outside the case reaches the what-if")
	}
}

func dropProposal(index string) Proposal {
	return Proposal{Type: ProposeIndexDrop, Index: index, Evidence: []string{"T1"},
		Rationale: "redundant"}
}

func TestJudge_IndexDropOfARedundantIndex(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, dropProposal("public.orders_customer_idx"))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	if f.Category != CategoryIndexDrop || f.ObjectIdentifier != "public.orders_customer_idx" {
		t.Fatalf("finding = %+v", f)
	}
	if f.RecommendedSQL != "DROP INDEX CONCURRENTLY IF EXISTS public.orders_customer_idx" {
		t.Fatalf("sql = %q", f.RecommendedSQL)
	}
	if f.RollbackSQL != "CREATE INDEX CONCURRENTLY orders_customer_idx ON public.orders "+
		"USING btree (customer_id)" {
		t.Fatalf("rollback (soft drop keeps the definition) = %q", f.RollbackSQL)
	}
	if f.Detail["covered_by"] != "orders_customer_status_idx" ||
		f.Detail["table"] != "public.orders" {
		t.Fatalf("detail = %v", f.Detail)
	}
	p := j.Prediction
	if p.Method != verify.MethodRule || *p.ExpectedChangePct != 0 ||
		j.Class != verify.ClassIndexDrop {
		t.Fatalf("a drop predicts reads unchanged: %+v", p)
	}
}

func TestJudge_IndexDropOfAnUnusedIndexNeedsLongStatistics(t *testing.T) {
	h := newHarness(t)
	if j := judgeOne(t, h, nil, dropProposal("public.orders_status_idx")); j.Verdict !=
		VerdictAdmitted {
		t.Fatalf("never scanned in 30 days of statistics: %+v", j)
	}
	cur := validationSnap()
	cur.System.RelationStatsEpoch = cur.CollectedAt.Add(-24 * time.Hour)
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0), nil, nil)
	j := v.judge(context.Background(), ordersCase(), ordersEvidence(),
		dropProposal("public.orders_status_idx"))
	if j.Verdict != VerdictRejected || j.Reason != ReasonInvalid {
		t.Fatalf("one day of statistics is not a business cycle: %+v", j)
	}
	cur.System.RelationStatsEpoch = time.Time{}
	j = v.judge(context.Background(), ordersCase(), ordersEvidence(),
		dropProposal("public.orders_status_idx"))
	if j.Reason != ReasonInvalid {
		t.Fatalf("an unknown statistics age refuses: %+v", j)
	}
}

func TestJudge_IndexDropRefusals(t *testing.T) {
	h := newHarness(t)
	for name, idx := range map[string]string{
		"primary key":          "public.orders_pkey",
		"unique":               "public.orders_ref_key",
		"scanned, not covered": "public.orders_customer_status_idx",
		"unknown":              "public.nope_idx",
	} {
		t.Run(name, func(t *testing.T) {
			j := judgeOne(t, h, nil, dropProposal(idx))
			if j.Verdict != VerdictRejected || j.Reason != ReasonInvalid || j.Finding != nil {
				t.Fatalf("judged = %+v", j)
			}
		})
	}
}

func TestJudge_BindingFactsRedirectToASourceFix(t *testing.T) {
	h := newHarness(t)
	managed := confirmedFact(12, facts.TypeAppMigrations, facts.KindTable,
		"public.orders", nil)
	j := judgeOne(t, h, []facts.Fact{managed}, createProposal())
	if j.Verdict != VerdictRedirected || j.Finding == nil {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("a bound proposal is never executable: %+v", f)
	}
	fix, ok := f.Detail["source_fix"].(facts.SourceFix)
	if !ok || fix.FactID != 12 || fix.Route != string(facts.RouteSourceFix) ||
		!strings.Contains(fix.Migration, "orders_customer_created_idx") {
		t.Fatalf("source fix = %#v", f.Detail["source_fix"])
	}
	appendOnly := confirmedFact(13, facts.TypeAppendOnly, facts.KindTable,
		"public.orders", nil)
	j = judgeOne(t, h, []facts.Fact{appendOnly}, dropProposal("public.orders_customer_idx"))
	if j.Verdict != VerdictRedirected || j.Finding.RecommendedSQL != "" {
		t.Fatalf("an append-only table keeps its indexes: %+v", j)
	}
}

func TestJudge_TestFixtureFactRejectsAsNotWorkload(t *testing.T) {
	h := newHarness(t)
	fixture := confirmedFact(14, facts.TypeTestFixture, facts.KindSchema, "public", nil)
	j := judgeOne(t, h, []facts.Fact{fixture}, dropProposal("public.orders_customer_idx"))
	if j.Verdict != VerdictRejected || j.Reason != ReasonNotWorkload {
		t.Fatalf("judged = %+v", j)
	}
}

func TestJudge_OperatorRejectedSQLIsNotReproposed(t *testing.T) {
	h := newHarness(t)
	h.store.rejected[normalizeSQL(
		"DROP INDEX CONCURRENTLY IF EXISTS public.orders_customer_idx;")] = true
	j := judgeOne(t, h, nil, dropProposal("public.orders_customer_idx"))
	if j.Verdict != VerdictRejected || j.Reason != ReasonOperatorRejected {
		t.Fatalf("judged = %+v", j)
	}
}

var _ = analyzer.DetailApprovalRequired
