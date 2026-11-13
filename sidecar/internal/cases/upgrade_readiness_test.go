package cases

import "testing"

// market-05 CHECK-04: an upgrade_readiness case is advisory. Even a
// finding that carried SQL gets no action candidate: the upgrade is the
// operator's, never an executor path.
func TestProjectFinding_UpgradeReadinessHasNoActionCandidate(t *testing.T) {
	f := SourceFinding{
		ID:               "77",
		DatabaseName:     "prod",
		Category:         "upgrade_readiness",
		Severity:         SeverityWarning,
		ObjectType:       "server",
		ObjectIdentifier: "PostgreSQL 14 to 18 via pg_upgrade: U1",
		Title:            "Upgrade to PostgreSQL 18: Tables without a primary key (block)",
		Recommendation:   "Add a primary key",
		RecommendedSQL:   "ANALYZE public.orders",
	}
	got := projectFinding(f)
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("upgrade_readiness candidates = %+v, want none", got.ActionCandidates)
	}
	if got.Title != f.Title || got.SourceType != SourceFindingType {
		t.Fatalf("case = %+v, want the finding's case", got)
	}
	f.Category = "stale_stats" // the same SQL on another category does get one
	if len(projectFinding(f).ActionCandidates) == 0 {
		t.Fatal("control: a stale_stats finding with ANALYZE should get a candidate")
	}
}
