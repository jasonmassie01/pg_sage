package optimizer

import "testing"

func TestRiskTierForRecommendation_ActionLevelsMapToModerate(t *testing.T) {
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
				DDL:         "CREATE INDEX CONCURRENTLY idx ON t (id)",
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

func TestRiskTierForRecommendation_ExplicitRiskPassesThrough(t *testing.T) {
	tests := []string{RiskSafe, RiskModerate, RiskHigh}

	for _, want := range tests {
		t.Run(want, func(t *testing.T) {
			rec := Recommendation{
				DDL:        "CREATE INDEX CONCURRENTLY idx ON t (id)",
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
	rec := Recommendation{
		DDL:        "CREATE INDEX CONCURRENTLY idx ON t (id)",
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
