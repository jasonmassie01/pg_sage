package earned

import (
	"strings"
	"testing"
	"time"
)

// earned-10: the newest bench report decides a family's bench checks
// whatever its origin (an air-gapped install runs on operator reports),
// so the proposal says which report decided it and where that report
// came from, and the Trust row carries the same report.
//
// No concurrent-access test here: the reason is a pure function of the
// evidence a proposal already snapshots; concurrent proposals are pinned
// by TestConcurrentApprovalsPromoteOnce.

// proposedReason is the reason of the pair's one promotion_proposed event.
func (f *fixture) proposedReason(family Family, class ActionClass) string {
	f.t.Helper()
	evs := f.events(EventFilter{Family: family, Class: class})
	var reasons []string
	for _, e := range evs {
		if e.Type == EventPromotionProposed {
			reasons = append(reasons, e.Reason)
		}
	}
	if len(reasons) != 1 {
		f.t.Fatalf("promotion_proposed reasons for %s/%s = %q, want one", family, class,
			reasons)
	}
	return reasons[0]
}

// decidingBench is the report the pair's bench checks read now.
func (f *fixture) decidingBench(family Family, class ActionClass) *EvalRun {
	f.t.Helper()
	ev, err := f.svc.Evidence(f.ctx, family, class)
	if err != nil {
		f.t.Fatalf("evidence: %v", err)
	}
	if ev.Bench == nil {
		f.t.Fatalf("no deciding bench report for %s/%s", family, class)
	}
	return ev.Bench
}

func TestProposalReasonNamesANewerOperatorReport(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	// An older signed release report, then the operator's newer unsigned one.
	if _, err := f.ingestSigned(stampedReport(fixtureEpoch.Add(-3*time.Hour), buildA,
		FamilyWAL), commitA); err != nil {
		t.Fatalf("ingest signed: %v", err)
	}
	f.seedL2Evidence(FamilyWAL, 25, 0)
	bench := f.decidingBench(FamilyWAL, ClassWALBound)
	if bench.Origin != OriginOperator {
		t.Fatalf("deciding report origin = %q: newest wins regardless of origin", bench.Origin)
	}
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	want := "evidence supports L2; bench: unsigned (operator-provided) " + bench.ID
	if got := f.proposedReason(FamilyWAL, ClassWALBound); got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func TestProposalReasonNamesANewerSignedReport(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	signed, err := f.ingestSigned(stampedReport(fixtureEpoch.Add(-30*time.Minute), buildA,
		FamilyWAL), commitA)
	if err != nil {
		t.Fatalf("ingest signed: %v", err)
	}
	if bench := f.decidingBench(FamilyWAL, ClassWALBound); bench.ID != signed.ID {
		t.Fatalf("deciding report = %s, want the newer signed %s", bench.ID, signed.ID)
	}
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := f.proposedReason(FamilyWAL, ClassWALBound)
	want := "evidence supports L2; bench: signed release report (pg_sage 1.8.5, " +
		"commit aaaaaaa) " + signed.ID
	if got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

// A self-initiated class has no bench evidence: its reason is unchanged.
func TestProposalReasonWithoutABenchReportIsUnchanged(t *testing.T) {
	f := newShadowFixture(t)
	for _, fp := range []string{"a", "b", "c"} {
		f.scoredShadow(ClassIndexCreate, "fp-nobench-"+fp, "correct", "hypopg", true)
	}
	f.reconcile()
	if p := evaluateCreated(t, f, ClassIndexCreate); p == nil || p.To != L2 {
		t.Fatalf("no L2 proposal from shadow evidence: %+v", p)
	}
	if got := f.proposedReason(FamilyTuning, ClassIndexCreate); got != "evidence supports L2" {
		t.Fatalf("reason without a bench report = %q, want %q", got, "evidence supports L2")
	}
}

// The Trust row carries the deciding report with its origin and label;
// a self-initiated row has none.
func TestTrustRowCarriesTheDecidingBenchReport(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	bench := f.decidingBench(FamilyWAL, ClassWALBound)
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var walRow, selfRow *TrustRow
	for i := range v.Rows {
		switch r := &v.Rows[i]; {
		case r.Family == FamilyWAL && r.Class == ClassWALBound:
			walRow = r
		case r.Family == FamilyTuning && r.Class == ClassIndexCreate:
			selfRow = r
		}
	}
	if walRow == nil || selfRow == nil {
		t.Fatalf("trust rows missing: wal %v, tuning %v", walRow, selfRow)
	}
	if b := walRow.Bench; b == nil || b.ID != bench.ID || b.Origin != OriginOperator ||
		b.Provenance != "unsigned (operator-provided)" || b.Signed {
		t.Fatalf("wal_bound row bench = %+v, want the operator report %s", b, bench.ID)
	}
	// A family the report did not score has no deciding report.
	for _, r := range v.Rows {
		if r.Family == FamilyLockBlocking && r.Bench != nil {
			t.Fatalf("lock_blocking row bench = %+v, want none", r.Bench)
		}
	}
	if selfRow.Bench != nil {
		t.Fatalf("self-initiated row bench = %+v, want none", selfRow.Bench)
	}
}

func TestProposalReasonFormat(t *testing.T) {
	run := &EvalRun{ID: "r1", Origin: OriginLocalRun, Build: buildA}
	for name, tc := range map[string]struct {
		to    Level
		bench *EvalRun
		want  string
	}{
		"no bench": {L2, nil, "evidence supports L2"},
		"local run": {L3, run,
			"evidence supports L3; bench: local run (pg_sage 1.8.5, commit aaaaaaa) r1"},
		"game day": {L2, &EvalRun{ID: "g", Origin: OriginGameDay},
			"evidence supports L2; bench: game day g"},
		"no origin": {L2, &EvalRun{ID: "x"},
			"evidence supports L2; bench: unsigned (operator-provided) x"},
		"empty id": {L2, &EvalRun{Origin: OriginOperator},
			"evidence supports L2; bench: unsigned (operator-provided)"},
		// The label comes from the origin, never from a stored label string.
		"bogus label": {L2, &EvalRun{ID: "y", Origin: "rogue",
			Provenance: "signed release report"},
			"evidence supports L2; bench: unsigned (operator-provided) y"},
	} {
		if got := proposalReason(tc.to, tc.bench); got != tc.want {
			t.Errorf("%s: reason = %q, want %q", name, got, tc.want)
		}
		if strings.HasSuffix(proposalReason(tc.to, tc.bench), " ") {
			t.Errorf("%s: trailing space", name)
		}
	}
}
