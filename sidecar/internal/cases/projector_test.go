package cases

import (
	"testing"
	"time"
)

func TestIdentityKeyFindingUsesStableProblemFields(t *testing.T) {
	f := SourceFinding{
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_no_primary_key",
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		RuleID:           "lint_no_primary_key",
	}

	got := IdentityKeyForFinding(f)
	want := "finding:prod:schema_lint:lint_no_primary_key:table:public.orders:lint_no_primary_key"
	if got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestIdentityKeyQueryPrefersNormalizedFingerprint(t *testing.T) {
	f := SourceFinding{
		DatabaseName:     "prod",
		Category:         "query_tuning",
		ObjectType:       "query",
		ObjectIdentifier: "query_id:123",
		Detail: map[string]any{
			"normalized_query": "select * from orders where id = ?",
		},
	}

	got := IdentityKeyForFinding(f)
	want := "finding:prod:query_tuning:query:select * from orders where id = ?"
	if got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestNewCaseRequiresWhyNowEvenWhenNotUrgent(t *testing.T) {
	c := NewCase(CaseInput{
		SourceType:   SourceFindingType,
		SourceID:     "42",
		DatabaseName: "prod",
		IdentityKey:  "finding:prod:test",
		Title:        "Test case",
		Severity:     SeverityInfo,
		Evidence: []Evidence{{
			Type:    "finding",
			Summary: "test evidence",
		}},
	})

	if c.WhyNow != "not urgent" {
		t.Fatalf("WhyNow = %q, want not urgent", c.WhyNow)
	}
	if c.State != StateOpen {
		t.Fatalf("State = %q, want %q", c.State, StateOpen)
	}
}

func TestProjectFindingCreatesActionableCase(t *testing.T) {
	f := SourceFinding{
		ID:               "42",
		DatabaseName:     "prod",
		Category:         "stale_stats",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "Stats are stale",
		Recommendation:   "Run ANALYZE on public.orders",
		RecommendedSQL:   "ANALYZE public.orders",
		Detail: map[string]any{
			"n_mod_since_analyze": float64(200000),
			"last_analyze_age":    "72h",
		},
	}

	got := ProjectFinding(f)

	if got.SourceType != SourceFindingType {
		t.Fatalf("SourceType = %q", got.SourceType)
	}
	if got.ActionCandidates[0].ActionType != "analyze_table" {
		t.Fatalf("ActionType = %q", got.ActionCandidates[0].ActionType)
	}
	if got.ActionCandidates[0].RiskTier != "safe" {
		t.Fatalf("RiskTier = %q", got.ActionCandidates[0].RiskTier)
	}
	if got.WhyNow == "not urgent" {
		t.Fatalf("WhyNow was not populated from stale-stat detail")
	}
}

func TestProjectFindingInformationalWhenNoRemediation(t *testing.T) {
	f := SourceFinding{
		ID:               "99",
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_serial_usage",
		Severity:         SeverityInfo,
		ObjectType:       "column",
		ObjectIdentifier: "public.orders.id",
		Title:            "Legacy serial usage",
		Recommendation:   "Prefer identity columns for new schema.",
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected no action candidates, got %d", len(got.ActionCandidates))
	}
	if got.State != StateOpen {
		t.Fatalf("State = %q", got.State)
	}
}

func TestResolveEphemeralWhenEvidenceDisappears(t *testing.T) {
	open := NewCase(CaseInput{
		SourceType:   SourceFindingType,
		SourceID:     "1",
		DatabaseName: "prod",
		IdentityKey:  "finding:prod:lock",
		Title:        "Lock pileup",
		Severity:     SeverityWarning,
		Evidence:     []Evidence{{Type: "lock", Summary: "blocked sessions"}},
		ActionCandidates: []ActionCandidate{{
			ActionType: "cancel_backend",
			RiskTier:   "moderate",
		}},
	})

	got := ResolveIfEvidenceMissing(open, false)

	if got.State != StateResolvedEphemeral {
		t.Fatalf("State = %q, want %q", got.State, StateResolvedEphemeral)
	}
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected pending candidates to clear")
	}
}

func TestExpiredActionCannotExecuteWithoutRevalidation(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	c := ActionCandidate{
		ActionType: "analyze_table",
		RiskTier:   "safe",
		ExpiresAt:  &expired,
	}

	if c.IsExecutable(time.Now()) {
		t.Fatalf("expired action should not be executable")
	}
}
