package firstlook

import (
	"testing"
	"time"
)

// Integration: the item counts LatestSummary takes in SQL are exactly
// CountItems of the decoded report, for every kind of item.
func TestLatestSummaryCountsItemsLikeCountItems(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("lsum_counts_")
	cleanReports(t, ctx, s, db)
	start := time.Now().UTC().Truncate(time.Millisecond)
	r := Report{Database: db, StartedAt: start, FinishedAt: start,
		Items: append(setupOnlyItems(),
			Item{Rule: RuleDuplicateIndex, Severity: SeverityWarning, Object: "public.a"},
			Item{Rule: RuleSequenceRunway, Severity: SeverityCritical, Object: "public.s"},
			Item{Rule: RuleRedundantIndex, Severity: SeverityInfo, Object: "public.b"},
			Item{Rule: RuleLogRCA, Severity: SeverityInfo, Object: CapLogRCA},
			Item{Rule: "AP-01", Section: SectionAgentPosture, Severity: SeverityCritical,
				Object: "sage_agent"})}
	if _, err := s.Save(ctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}
	full, found, err := s.Latest(ctx, db)
	if err != nil || !found {
		t.Fatalf("latest: found=%v err=%v", found, err)
	}
	got, found, err := s.LatestSummary(ctx, db)
	if err != nil || !found {
		t.Fatalf("latest summary: found=%v err=%v", found, err)
	}
	want := ItemCounts{Findings: 2, Critical: 1, Setup: 5, Posture: 1, Notes: 1}
	if got.Counts != want || got.Counts != CountItems(full.Items) {
		t.Fatalf("LatestSummary counts = %+v, want %+v (CountItems %+v)", got.Counts, want,
			CountItems(full.Items))
	}
}

// Nil/empty: a row with no items counts zero of everything.
func TestLatestSummaryCountsOfAnEmptyRow(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("lsum_counts_empty_")
	cleanReports(t, ctx, s, db)
	insertRow(t, s, `INSERT INTO sage.first_look (database_name, started_at,
		finished_at) VALUES ($1, now(), now())`, db)
	got, found, err := s.LatestSummary(ctx, db)
	if err != nil || !found || got.Counts != (ItemCounts{}) {
		t.Fatalf("empty row: counts %+v found=%v err=%v", got.Counts, found, err)
	}
}
