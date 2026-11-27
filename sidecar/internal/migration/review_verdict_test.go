package migration

import (
	"strings"
	"testing"
)

// No concurrent access tests: finishReport and hazardVerdict are pure.
// These carry agenttools' reviewUnproven cases (migration-04a), which moved
// here with the logic when lint became a wrapper over Review.

func reportOf(verdicts ...string) Report {
	rep := Report{}
	for _, v := range verdicts {
		rep.Statements = append(rep.Statements, StatementReview{Verdict: v,
			Hazards: []Hazard{{RiskScore: 0.9}}})
	}
	return rep
}

func TestFinishReportUnrecognizedRaisesSafeToReview(t *testing.T) {
	rep := Report{Statements: []StatementReview{{Verdict: VerdictReview}}}
	finishReport(&rep, Classification{Statements: []ClassifiedStatement{
		{Status: StatementUnrecognized}}})
	if rep.Verdict != VerdictReview || !strings.Contains(rep.Notes[0], "1 statement") ||
		!strings.Contains(rep.Notes[0], "known safe form") {
		t.Fatalf("report %+v", rep)
	}
	rep = Report{}
	finishReport(&rep, Classification{Degraded: true, Statements: []ClassifiedStatement{
		{Status: StatementUnrecognized}, {Status: StatementUnrecognized}}})
	if !strings.Contains(rep.Notes[0], "2 statements") ||
		!strings.Contains(rep.Notes[0], "allowlist") {
		t.Fatalf("degraded note %v", rep.Notes)
	}
}

func TestFinishReportUntokenizableIsReview(t *testing.T) {
	rep := Report{}
	finishReport(&rep, Classification{Untokenizable: true})
	if rep.Verdict != VerdictReview || !strings.Contains(rep.Notes[0], "tokenize") {
		t.Fatalf("report %+v", rep)
	}
}

// It never lowers a verdict and keeps the highest score.
func TestFinishReportKeepsRisky(t *testing.T) {
	rep := reportOf(VerdictReview, VerdictRisky)
	finishReport(&rep, Classification{Untokenizable: true})
	if rep.Verdict != VerdictRisky || rep.MaxRiskScore != 0.9 {
		t.Fatalf("report %+v", rep)
	}
	rep = Report{}
	finishReport(&rep, Classification{})
	if rep.Verdict != VerdictSafe || len(rep.Notes) != 0 {
		t.Fatalf("empty report %+v", rep)
	}
}

// Boundary on the score: 0.7 is risky whatever the size; just below it,
// an unknown size is review.
func TestHazardVerdictScoreBoundary(t *testing.T) {
	r := &DDLRisk{LockLevel: "SHARE", RuleID: "ddl_index_not_concurrent", RiskScore: 0.7}
	if got := hazardVerdict(r, 10000, false); got != VerdictRisky {
		t.Fatalf("score 0.7: %s", got)
	}
	r.RiskScore = 0.6999
	if got := hazardVerdict(r, 10000, false); got != VerdictReview {
		t.Fatalf("score 0.6999, size unknown: %s", got)
	}
	r.StatsKnown, r.EstimatedRows = true, 10000
	if got := hazardVerdict(r, 10000, true); got != VerdictRisky {
		t.Fatalf("index build at the threshold: %s", got)
	}
	weak := &DDLRisk{LockLevel: "SHARE UPDATE EXCLUSIVE", StatsKnown: true,
		EstimatedRows: 1 << 30, RiskScore: 0.3}
	if got := hazardVerdict(weak, 10000, false); got != VerdictReview {
		t.Fatalf("a lock that does not block writes: %s", got)
	}
}
