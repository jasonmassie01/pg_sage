package optimizer

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// indexes-02: the validator's gates treated missing data as negative
// evidence. The tuning agent never sets EstimatedImprovementPct, so the
// write-impact gate compared 0 with 15% and rejected every candidate on a
// write-heavy table before HypoPG ran. The gate now reads the what-if's
// measurement: only a complete measurement below the minimum rejects.

func writeHeavyTable(rate float64) TableContext {
	tc := admitTable()
	tc.WriteRate, tc.Workload, tc.WriteRateKnown = rate, classifyWorkload(rate, tc.LiveTuples),
		true
	return tc
}

func measured(pct float64) WhatIfResult {
	return WhatIfResult{Measured: 2, Improvement: pct, SizeBytes: 8192}
}

// Fails before the fix: "write-heavy table with low estimated improvement"
// without a what-if.
func TestAdmit_WriteHeavyMeasuredWellIsAccepted(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), measured(40))
	a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(90))
	if a.Outcome != AdmitAccepted || w.calls.Load() != 1 {
		t.Fatalf("admission = %+v (what-ifs %d), want admitted after one what-if", a,
			w.calls.Load())
	}
	if a.Rec.WhatIf != WhatIfVerified || !a.Rec.Validated ||
		a.Rec.EstimatedImprovementPct != 40 || a.Rec.WhatIfReason != "" {
		t.Fatalf("rec = %+v, want verified at 40%%", a.Rec)
	}
}

func TestAdmit_WriteHeavyBelowHypoPGMinimumIsRejectedOnce(t *testing.T) {
	store := newMemStore()
	o, w, _ := admissionOptimizer(store, measured(5))
	a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(90))
	if a.Outcome != AdmitRejected || w.calls.Load() != 1 || store.records != 1 {
		t.Fatalf("admission = %+v (what-ifs %d, records %d)", a, w.calls.Load(),
			store.records)
	}
	if !strings.Contains(a.Reason, "5.0%") || a.Rec.WhatIf != WhatIfRejected {
		t.Fatalf("reason = %q verdict %q", a.Reason, a.Rec.WhatIf)
	}
}

// Between the HypoPG minimum (10) and the write-impact minimum (15): the
// measurement is complete, so the write-impact gate rejects, says why, and
// rejection memory keeps the idea from being measured again.
func TestAdmit_WriteHeavyBelowWriteImpactMinimumIsRejectedAndRemembered(t *testing.T) {
	store := newMemStore()
	o, w, _ := admissionOptimizer(store, measured(12))
	tc := writeHeavyTable(90)
	a := o.Admit(context.Background(), candidate(statusDDL), tc)
	if a.Outcome != AdmitRejected || a.Rec.WhatIf != WhatIfRejected || a.Rec.Validated {
		t.Fatalf("admission = %+v, want a what-if rejection", a)
	}
	for _, want := range []string{"write-heavy", "90.0%", "12.0%", "15.0%"} {
		if !strings.Contains(a.Reason, want) {
			t.Errorf("reason %q lacks %q", a.Reason, want)
		}
	}
	if store.records != 1 {
		t.Fatalf("records = %d, want the rejection remembered", store.records)
	}
	again := o.Admit(context.Background(), candidate(statusDDL), tc)
	if again.Outcome != AdmitMeasured || w.calls.Load() != 1 {
		t.Fatalf("second admission = %+v (what-ifs %d), want memory to skip it", again,
			w.calls.Load())
	}
	lines := o.MeasuredRejections(context.Background(), tc)
	if len(lines) != 1 || !strings.Contains(lines[0], "below the 15.0% minimum") {
		t.Fatalf("case packet lines = %q, want the write-impact minimum", lines)
	}
}

// State transition: the remembered write-impact rejection binds only while
// the table is write-heavy; once it is not, the idea is measured again.
func TestAdmit_WriteImpactRejectionLapsesWhenTheTableStopsBeingWriteHeavy(t *testing.T) {
	store := newMemStore()
	o, w, _ := admissionOptimizer(store, measured(12))
	if a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(90)); a.
		Outcome != AdmitRejected {
		t.Fatalf("write-heavy admission = %+v", a)
	}
	read := writeHeavyTable(20)
	a := o.Admit(context.Background(), candidate(statusDDL), read)
	if a.Outcome != AdmitAccepted || w.calls.Load() != 2 || a.Rec.WhatIf != WhatIfVerified {
		t.Fatalf("read-mostly admission = %+v (what-ifs %d), want re-measured and verified",
			a, w.calls.Load())
	}
	if lines := o.MeasuredRejections(context.Background(), read); len(lines) != 0 {
		t.Fatalf("case packet still lists %q on a read-mostly table", lines)
	}
}

func TestAdmit_WriteHeavyUnverifiedStaysAcceptedUnverified(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	o.whatIf = unavailableWhatIf{}
	a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(90))
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfUnverified || a.Rec.Validated {
		t.Fatalf("admission = %+v, want accepted unverified (operator gate)", a)
	}
	want := "HypoPG unavailable; write-heavy table; improvement unmeasured"
	if a.Rec.WhatIfReason != want {
		t.Fatalf("reason = %q, want %q", a.Rec.WhatIfReason, want)
	}
}

func TestAdmit_PartialMeasurementOnAWriteHeavyTableStaysUnverified(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 1, Failed: 1,
		Improvement: 3, SizeBytes: 8192, UnplannedQueryIDs: []int64{2}})
	a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(95))
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfUnverified ||
		!strings.HasSuffix(a.Rec.WhatIfReason, "; write-heavy table; improvement unmeasured") {
		t.Fatalf("admission = %+v: only a complete measurement rejects", a)
	}
}

func TestAdmit_ReadTableUnverifiedReasonIsUnchanged(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	o.whatIf = unavailableWhatIf{}
	a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(5))
	if a.Rec.WhatIfReason != "HypoPG unavailable" {
		t.Fatalf("reason = %q, want only the HypoPG reason", a.Rec.WhatIfReason)
	}
}

// Boundary: write-heavy means strictly above write_heavy_ratio_pct (70),
// and an improvement equal to write_impact_threshold_pct (15) passes.
func TestAdmit_WriteHeavyBoundaries(t *testing.T) {
	for _, c := range []struct {
		rate, pct float64
		want      AdmissionOutcome
	}{
		{70.00, 12, AdmitAccepted},
		{70.01, 12, AdmitRejected},
		{90, 15, AdmitAccepted},
		{90, 14.99, AdmitRejected},
		{0, 10, AdmitAccepted},
	} {
		o, _, _ := admissionOptimizer(newMemStore(), measured(c.pct))
		a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(c.rate))
		if a.Outcome != c.want {
			t.Errorf("rate %.2f improvement %.2f: %s (%s), want %s", c.rate, c.pct,
				a.Outcome, a.Reason, c.want)
		}
	}
}

// The write-impact minimum falls back to 15 when unset (zero config).
func TestAdmit_WriteImpactDefaultThreshold(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), measured(14))
	o.cfg.WriteImpactThreshPct = 0
	a := o.Admit(context.Background(), candidate(statusDDL), writeHeavyTable(90))
	if a.Outcome != AdmitRejected || !strings.Contains(a.Reason, "15.0%") {
		t.Fatalf("admission = %+v, want rejected under the default 15%% minimum", a)
	}
}

// Concurrency: one optimizer admits candidates for several tables at once
// (the tuning agent's cases); each outcome follows its own table.
// -race is unavailable on this machine; crossed outcomes still fail.
func TestAdmit_ConcurrentWriteHeavyDecisions(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), measured(12))
	var wg sync.WaitGroup
	got := make([]AdmissionOutcome, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tc := writeHeavyTable(20)
			if i%2 == 0 {
				tc = writeHeavyTable(90)
				tc.Table = "ai_claims_heavy"
			}
			got[i] = o.Admit(context.Background(), candidate(strings.Replace(statusDDL,
				"public.ai_claims", "public."+tc.Table, 1)), tc).Outcome
		}()
	}
	wg.Wait()
	for i, outcome := range got {
		want := AdmitAccepted
		if i%2 == 0 {
			want = AdmitRejected
		}
		if outcome != want && !(i%2 == 0 && outcome == AdmitMeasured) {
			t.Errorf("candidate %d: %s, want %s", i, outcome, want)
		}
	}
}

// Column metadata missing this cycle (a timed-out catalog read) is not
// evidence that a column does not exist. Fails before the fix: "column
// status does not exist".
func TestAdmit_UnknownColumnsNeverSayDoesNotExist(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	o.whatIf = unavailableWhatIf{}
	tc := admitTable()
	tc.Columns, tc.ColumnsKnown = nil, false
	a := o.Admit(context.Background(), candidate(statusDDL), tc)
	if a.Outcome != AdmitAccepted || strings.Contains(a.Reason, "does not exist") {
		t.Fatalf("admission = %+v, want accepted without column evidence", a)
	}
	want := "HypoPG unavailable; column metadata unavailable this cycle"
	if a.Rec.WhatIf != WhatIfUnverified || a.Rec.WhatIfReason != want {
		t.Fatalf("verdict %q reason %q, want unverified %q", a.Rec.WhatIf,
			a.Rec.WhatIfReason, want)
	}
	if w.calls.Load() != 0 {
		t.Fatalf("what-ifs = %d", w.calls.Load())
	}
}

// A complete HypoPG measurement planned the index, so its columns exist:
// the verdict stays verified without the column-metadata note.
func TestAdmit_UnknownColumnsVerifiedByHypoPG(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), measured(30))
	tc := admitTable()
	tc.Columns, tc.ColumnsKnown = nil, false
	a := o.Admit(context.Background(), candidate(statusDDL), tc)
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfVerified || a.Rec.WhatIfReason != "" {
		t.Fatalf("admission = %+v, want verified", a)
	}
}

func TestAdmit_KnownColumnsStillRejectAMissingColumn(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), measured(30))
	a := o.Admit(context.Background(), candidate(
		"CREATE INDEX CONCURRENTLY x ON public.ai_claims (no_such_column)"), admitTable())
	if a.Outcome != AdmitInvalid || a.Reason != "column no_such_column does not exist" ||
		w.calls.Load() != 0 {
		t.Fatalf("admission = %+v (what-ifs %d)", a, w.calls.Load())
	}
}

// Nil and empty: an empty table context (nothing read) fails no column
// check; a zero-value context is not write-heavy.
func TestValidate_EmptyContextIsNotNegativeEvidence(t *testing.T) {
	v := newTestValidator(fnTestOptimizerConfig())
	for _, tc := range []TableContext{{}, {Columns: []ColumnInfo{}}} {
		ok, reason := v.checkColumnExistence(Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY i ON public.t (a) INCLUDE (b)"}, tc)
		if !ok || reason != "" {
			t.Fatalf("columns unknown: ok=%t reason %q", ok, reason)
		}
	}
	o, _, _ := admissionOptimizer(newMemStore(), measured(1))
	if o.validator.writeHeavy(TableContext{}) {
		t.Fatal("a zero-value context reads as write-heavy")
	}
}
