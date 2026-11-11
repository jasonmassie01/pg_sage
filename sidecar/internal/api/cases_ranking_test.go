package api

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// Within a severity, a case with an assessed time to impact ranks before
// one without, the nearer impact first; severity always dominates, and
// recency, database and id break the remaining ties (cases-07).
// No concurrent access tests: sortCasesGlobally sorts one request's slice.

var rankAt = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func rankCase(id string, sev cases.Severity, observed time.Duration, tti float64) cases.Case {
	c := cases.Case{ID: id, DatabaseName: "prod", Severity: sev,
		ObservedAt: rankAt.Add(-observed)}
	if tti > 0 {
		c.Urgency = cases.Urgency{Assessed: true, TimeToImpactSeconds: &tti}
	}
	return c
}

func rankedIDs(list []cases.Case) string {
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	return strings.Join(ids, ",")
}

func TestSortCases_TimeToImpactOrdersWithinASeverity(t *testing.T) {
	list := []cases.Case{
		// The 30-day warning is the freshest; the 3-day one still ranks first.
		rankCase("w30", cases.SeverityWarning, 0, 30*86400),
		rankCase("w3", cases.SeverityWarning, time.Hour, 3*86400),
	}
	sortCasesGlobally(list)
	if got := rankedIDs(list); got != "w3,w30" {
		t.Fatalf("order = %s, want w3,w30", got)
	}
}

func TestSortCases_AssessedBeforeUnassessedAndSeverityDominates(t *testing.T) {
	list := []cases.Case{
		rankCase("w-fresh-unassessed", cases.SeverityWarning, 0, 0),
		rankCase("w-assessed", cases.SeverityWarning, 48*time.Hour, 7*86400),
		rankCase("c-unassessed", cases.SeverityCritical, 72*time.Hour, 0),
		rankCase("i-1min", cases.SeverityInfo, 0, 60),
		rankCase("c-assessed", cases.SeverityCritical, 96*time.Hour, 86400),
	}
	sortCasesGlobally(list)
	want := "c-assessed,c-unassessed,w-assessed,w-fresh-unassessed,i-1min"
	if got := rankedIDs(list); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestSortCases_TiesFallBackToRecencyThenDatabaseThenID(t *testing.T) {
	a := rankCase("b", cases.SeverityWarning, time.Hour, 3600)
	b := rankCase("a", cases.SeverityWarning, 0, 3600)
	c := rankCase("c", cases.SeverityWarning, 0, 3600)
	c.DatabaseName = "alpha"
	d := rankCase("d", cases.SeverityWarning, 0, 0)
	e := rankCase("e", cases.SeverityWarning, time.Hour, 0)
	list := []cases.Case{e, a, d, b, c}
	sortCasesGlobally(list)
	if got := rankedIDs(list); got != "c,a,b,d,e" {
		t.Fatalf("order = %s, want c,a,b,d,e", got)
	}
}

// A malformed urgency (assessed without a usable number) ranks as
// unassessed rather than first.
func TestSortCases_MalformedUrgencyRanksAsUnassessed(t *testing.T) {
	nan := math.NaN()
	broken := rankCase("broken", cases.SeverityWarning, 0, 0)
	broken.Urgency = cases.Urgency{Assessed: true, TimeToImpactSeconds: &nan}
	noNumber := rankCase("no-number", cases.SeverityWarning, time.Minute, 0)
	noNumber.Urgency = cases.Urgency{Assessed: true}
	list := []cases.Case{broken, noNumber, rankCase("real", cases.SeverityWarning,
		time.Hour, 86400)}
	sortCasesGlobally(list)
	if got := rankedIDs(list); got != "real,broken,no-number" {
		t.Fatalf("order = %s, want real,broken,no-number", got)
	}
	if !strings.Contains(list[1].RankBasis, "not assessed") {
		t.Fatalf("rank basis = %q", list[1].RankBasis)
	}
}

func TestSortCases_EveryCaseCarriesItsRankBasis(t *testing.T) {
	list := []cases.Case{
		rankCase("w3", cases.SeverityWarning, 0, 3*86400),
		rankCase("w", cases.SeverityWarning, 0, 0),
	}
	sortCasesGlobally(list)
	if list[0].RankBasis != "severity warning; time to impact about 3 days" {
		t.Fatalf("assessed rank basis = %q", list[0].RankBasis)
	}
	if list[1].RankBasis != "severity warning; urgency not assessed; most recent first" {
		t.Fatalf("unassessed rank basis = %q", list[1].RankBasis)
	}
}

func TestSortCases_EmptyAndNil(t *testing.T) {
	sortCasesGlobally(nil)
	empty := []cases.Case{}
	sortCasesGlobally(empty)
	if len(empty) != 0 {
		t.Fatalf("empty list grew: %v", empty)
	}
}

// A case whose producer reports the limit already reached ranks before
// every projection of its severity, but never above a higher severity.
func TestSortCases_LimitReachedRanksFirstWithinItsSeverity(t *testing.T) {
	full := rankCase("full", cases.SeverityCritical, 2*time.Hour, 0)
	full.Urgency = cases.Urgency{LimitReached: true}
	fullWarn := rankCase("full-warn", cases.SeverityWarning, 0, 0)
	fullWarn.Urgency = cases.Urgency{LimitReached: true}
	list := []cases.Case{
		rankCase("c2h", cases.SeverityCritical, 0, 7200),
		fullWarn, full,
		rankCase("c", cases.SeverityCritical, 0, 0),
	}
	sortCasesGlobally(list)
	if got := rankedIDs(list); got != "full,c2h,c,full-warn" {
		t.Fatalf("order = %s, want full,c2h,c,full-warn", got)
	}
	if list[0].RankBasis != "severity critical; limit already reached" {
		t.Fatalf("rank basis = %q", list[0].RankBasis)
	}
}
