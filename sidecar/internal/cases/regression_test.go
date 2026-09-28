package cases

import (
	"testing"
	"time"
)

// Regression tests for case projection bugs recorded in
// reviews/2026-09-26 (G2-B24/SURF-11, C16).

// G2-B24/SURF-11: observed_at must come from the source finding and the
// candidate expiry must be anchored to evidence time, not to the request.
func TestRegression_CaseTimesAnchoredToEvidence(t *testing.T) {
	observed := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	f := SourceFinding{
		ID: "7", Category: "unused_index", Severity: SeverityWarning,
		ObjectType: "index", ObjectIdentifier: "public.idx_x",
		Title:          "Unused index public.idx_x",
		RecommendedSQL: `DROP INDEX CONCURRENTLY "public"."idx_x";`,
		ObservedAt:     observed,
	}
	c := ProjectFinding(f)
	if !c.ObservedAt.Equal(observed) {
		t.Fatalf("ObservedAt = %v, want %v", c.ObservedAt, observed)
	}
	if len(c.ActionCandidates) == 0 || c.ActionCandidates[0].ExpiresAt == nil {
		t.Fatalf("expected a candidate with expiry, got %+v", c.ActionCandidates)
	}
	want := observed.Add(24 * time.Hour)
	if got := *c.ActionCandidates[0].ExpiresAt; !got.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", got, want)
	}
	if c.ActionCandidates[0].IsExecutable(time.Now()) {
		t.Fatal("stale candidate reported executable")
	}
}

// G2-B24: vacuum-autopilot candidates are anchored the same way.
func TestRegression_VacuumCandidateExpiryAnchored(t *testing.T) {
	observed := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := ProjectFinding(SourceFinding{
		ID: "8", Category: "table_bloat", Severity: SeverityWarning,
		ObjectType: "table", ObjectIdentifier: "public.orders",
		Title: "bloat", ObservedAt: observed,
	})
	if len(c.ActionCandidates) != 1 || c.ActionCandidates[0].ExpiresAt == nil {
		t.Fatalf("candidates = %+v", c.ActionCandidates)
	}
	if got := *c.ActionCandidates[0].ExpiresAt; !got.Equal(observed.Add(24 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v", got)
	}
}

// C16: the fallback VACUUM statement must quote the identifier.
func TestRegression_TableBloatFallbackQuotes(t *testing.T) {
	c := ProjectFinding(SourceFinding{
		ID: "9", Category: "table_bloat", Severity: SeverityWarning,
		ObjectType: "table", ObjectIdentifier: "Sales.Orders", Title: "bloat",
	})
	want := `VACUUM "Sales"."Orders";`
	if len(c.ActionCandidates) != 1 || c.ActionCandidates[0].ProposedSQL != want {
		t.Fatalf("candidates = %+v, want SQL %s", c.ActionCandidates, want)
	}
}
