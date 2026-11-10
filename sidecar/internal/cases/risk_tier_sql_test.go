package cases

import "testing"

// llm-08: the briefing shows each recommendation's risk tier, taken from the
// same classifier the case projector uses for its action candidates.
func TestRiskTierForSQL(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"", ""},
		{"   \n", ""},
		{"VACUUM public.orders", "safe"},
		{"ANALYZE public.orders", "safe"},
		{"CREATE INDEX CONCURRENTLY idx ON public.orders (id)", "moderate"},
		{"DROP INDEX CONCURRENTLY public.idx", "moderate"},
		{"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.01)",
			"moderate"},
		{"REINDEX INDEX CONCURRENTLY public.idx", "moderate"},
		{"ALTER TABLE public.orders ADD COLUMN x int", "high"},
		{"ALTER SYSTEM SET work_mem = '64MB'", "moderate"},
		{"SELECT pg_terminate_backend(42)", "high"},
	}
	for _, tc := range cases {
		if got := RiskTierForSQL(tc.sql, testClassify); got != tc.want {
			t.Errorf("RiskTierForSQL(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

// The briefing and the case candidate must never disagree on a finding.
func TestRiskTierForSQLMatchesProjectedCandidate(t *testing.T) {
	f := SourceFinding{ID: "7", DatabaseName: "db", Category: "missing_index",
		ObjectType: "table", ObjectIdentifier: "public.orders", Severity: "warning",
		Title: "Missing index", RecommendedSQL: "CREATE INDEX CONCURRENTLY i ON t (a)"}
	c := projectFinding(f)
	if len(c.ActionCandidates) == 0 {
		t.Fatal("projected case has no action candidate")
	}
	got := RiskTierForSQL(f.RecommendedSQL, testClassify)
	if got != c.ActionCandidates[0].RiskTier {
		t.Fatalf("RiskTierForSQL = %q, candidate risk tier = %q", got,
			c.ActionCandidates[0].RiskTier)
	}
}
