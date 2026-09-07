package optimizer

import "testing"

func TestRiskTierForRecommendation_ActionLevelsMapToModerate(t *testing.T) {
	// Non-index-create DDL with no explicit risk falls back to the action
	// level, which maps to moderate for these levels.
	tests := []struct {
		name        string
		actionLevel string
		want        string
	}{
		{name: "autonomous", actionLevel: "autonomous", want: RiskModerate},
		{name: "advisory", actionLevel: "advisory", want: RiskModerate},
		{name: "informational", actionLevel: "informational", want: RiskModerate},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := Recommendation{
				DDL:         "DROP INDEX CONCURRENTLY idx",
				ActionLevel: tt.actionLevel,
			}

			got := RiskTierForRecommendation(rec)

			if got != tt.want {
				t.Fatalf("RiskTierForRecommendation(%q) = %q, want %q",
					tt.actionLevel, got, tt.want)
			}
		})
	}
}

func TestRiskTierForRecommendation_IndexCreateAlwaysModerate(t *testing.T) {
	// CREATE INDEX (any access method, any self-rated risk) is moderate so
	// it can run autonomously — online + reversible. This must override even
	// an LLM-inflated high_risk or an unknown self-rating.
	ddls := []string{
		"CREATE INDEX CONCURRENTLY idx ON t (id)",
		"CREATE INDEX CONCURRENTLY ON e USING gin (payload jsonb_path_ops)",
		"CREATE INDEX CONCURRENTLY ON d USING hnsw (embedding vector_l2_ops)",
		"CREATE UNIQUE INDEX CONCURRENTLY uq ON t (email)",
	}
	for _, risk := range []string{"", RiskSafe, RiskModerate, RiskHigh, "maybe"} {
		for _, ddl := range ddls {
			rec := Recommendation{DDL: ddl, ActionRisk: risk}
			if got := RiskTierForRecommendation(rec); got != RiskModerate {
				t.Fatalf("index create %q (self-rated %q) = %q, want moderate",
					ddl, risk, got)
			}
		}
	}
}

func TestRiskTierForRecommendation_ExplicitRiskPassesThrough(t *testing.T) {
	// For non-index-create DDL (e.g. DROP INDEX), the self-rated risk is
	// honored verbatim.
	tests := []string{RiskSafe, RiskModerate, RiskHigh}

	for _, want := range tests {
		t.Run(want, func(t *testing.T) {
			rec := Recommendation{
				DDL:        "DROP INDEX CONCURRENTLY idx",
				ActionRisk: want,
			}

			got := RiskTierForRecommendation(rec)

			if got != want {
				t.Fatalf("RiskTierForRecommendation explicit risk = %q, want %q",
					got, want)
			}
		})
	}
}

func TestRiskTierForRecommendation_UnknownRiskFailsClosed(t *testing.T) {
	// A non-index-create DDL with an unrecognized self-rating fails closed
	// to high_risk (advisory only).
	rec := Recommendation{
		DDL:        "DROP INDEX CONCURRENTLY idx",
		ActionRisk: "maybe_safe",
	}

	got := RiskTierForRecommendation(rec)

	if got != RiskHigh {
		t.Fatalf("unknown risk mapped to %q, want %q", got, RiskHigh)
	}
}

func TestRiskTierForRecommendation_NoDDLHasNoRisk(t *testing.T) {
	rec := Recommendation{ActionLevel: "advisory"}

	got := RiskTierForRecommendation(rec)

	if got != "" {
		t.Fatalf("risk for non-actionable recommendation = %q, want empty", got)
	}
}
