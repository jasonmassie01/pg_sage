package firstlook

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// No concurrent access tests: SummaryOf is a pure function of its report.

func summarySample() Report {
	r := sampleReport()
	r.ID, r.DurationMS, r.Summary = 7, 812, "Two findings."
	r.FinishedAt = time.Date(2026, 11, 8, 9, 0, 1, 0, time.UTC)
	r.Checks = []Check{
		{Rule: RuleDuplicateIndex, Status: CheckFinding},
		{Rule: RuleSequenceRunway, Status: CheckDegraded, Note: "GRANT SELECT"},
		{Rule: RuleNeverScannedIndex, Status: CheckFinding, Truncated: true},
		{Rule: "AP-01", Section: SectionAgentPosture, Status: CheckDegraded,
			Truncated: true},
	}
	r.Capabilities = []Capability{{Name: CapStatStatements, Status: CapabilityOK}}
	return r
}

func TestSummaryOfCountsCatalogChecksOnly(t *testing.T) {
	r := summarySample()
	got := SummaryOf(r)
	want := ReportSummary{ID: 7, Database: "app", FinishedAt: r.FinishedAt,
		DurationMS: 812, Summary: "Two findings.", Items: 2,
		Counts:       ItemCounts{Findings: 2, Critical: 1},
		Checks:       CheckCounts{Total: 3, Degraded: 1, Truncated: 1},
		Capabilities: r.Capabilities}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SummaryOf = %+v\nwant %+v", got, want)
	}
}

func TestSummaryOfEmptyReport(t *testing.T) {
	got := SummaryOf(Report{})
	if got.Items != 0 || got.Checks != (CheckCounts{}) || len(got.Capabilities) != 0 ||
		got.ID != 0 || !got.FinishedAt.IsZero() {
		t.Fatalf("SummaryOf(empty) = %+v", got)
	}
}

func TestLatestSummaryWithoutPool(t *testing.T) {
	for _, s := range []*Store{nil, NewStore(nil)} {
		_, found, err := s.LatestSummary(context.Background(), "app")
		if !errors.Is(err, ErrNoPool) || found {
			t.Fatalf("LatestSummary without a pool: found=%v err=%v, want ErrNoPool",
				found, err)
		}
	}
}
