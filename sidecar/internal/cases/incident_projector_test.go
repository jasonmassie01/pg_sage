package cases

import (
	"testing"
	"time"
)

func TestProjectIncidentCreatesHighUrgencyCase(t *testing.T) {
	incident := testIncident("prod")
	incident.Severity = SeverityCritical

	got := ProjectIncident(incident)

	if got.SourceType != SourceIncidentType {
		t.Fatalf("SourceType = %q", got.SourceType)
	}
	if got.DatabaseName != "prod" {
		t.Fatalf("DatabaseName = %q", got.DatabaseName)
	}
	if got.WhyNow != "critical incident requires immediate review" {
		t.Fatalf("WhyNow = %q", got.WhyNow)
	}
	if got.Evidence[0].Detail["confidence"] != incident.Confidence {
		t.Fatalf("confidence detail = %#v", got.Evidence[0].Detail)
	}
	if got.Evidence[0].Detail["occurrence_count"] != incident.OccurrenceCount {
		t.Fatalf("occurrence detail = %#v", got.Evidence[0].Detail)
	}
}

func TestIdleTxnIncidentAddsPlaybookCandidates(t *testing.T) {
	incident := testIncident("prod")

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 3 {
		t.Fatalf("ActionCandidates = %d, want 3", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_lock_blockers", "safe", "")
	assertCandidate(t, got.ActionCandidates[1],
		"cancel_backend", "moderate", "SELECT pg_cancel_backend(12345)")
	assertCandidate(t, got.ActionCandidates[2],
		"terminate_backend", "high", "SELECT pg_terminate_backend(12345)")
}

func TestIdleTxnPlaybookBlocksPidActionsWithoutSinglePID(t *testing.T) {
	incident := testIncident("prod")
	incident.RootCause = "Idle transactions are elevated"
	incident.SignalIDs = []string{"idle_in_tx_elevated", "connections_high"}
	incident.CausalChain = nil

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %d, want 2", len(got.ActionCandidates))
	}
	if got.ActionCandidates[1].BlockedReason != "blocker PID unavailable" {
		t.Fatalf("BlockedReason = %q", got.ActionCandidates[1].BlockedReason)
	}
	if got.ActionCandidates[1].ProposedSQL != "" {
		t.Fatalf("ProposedSQL = %q, want empty",
			got.ActionCandidates[1].ProposedSQL)
	}
}

func TestNonActionableIncidentHasNoCandidates(t *testing.T) {
	incident := SourceIncident{
		ID:           "inc-2",
		DatabaseName: "prod",
		Severity:     SeverityWarning,
		RootCause:    "WAL archive command failed",
		SignalIDs:    []string{"wal_archive_failed"},
		Source:       "logwatch",
		Confidence:   0.8,
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected no action candidates, got %d",
			len(got.ActionCandidates))
	}
}

func TestResolvedIncidentProjectsResolvedState(t *testing.T) {
	incident := testIncident("prod")
	resolved := time.Now().UTC()
	incident.ResolvedAt = &resolved

	got := ProjectIncident(incident)

	if got.State != StateResolved {
		t.Fatalf("State = %q, want resolved", got.State)
	}
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("resolved incident should not have candidates")
	}
}

func TestIncidentIdentityIncludesDatabase(t *testing.T) {
	a := ProjectIncident(testIncident("prod"))
	b := ProjectIncident(testIncident("stage"))

	if a.IdentityKey == b.IdentityKey {
		t.Fatalf("identity keys should differ: %q", a.IdentityKey)
	}
}

func testIncident(database string) SourceIncident {
	return SourceIncident{
		ID:              "inc-1",
		DatabaseName:    database,
		Severity:        SeverityWarning,
		RootCause:       "Idle-in-transaction PID 12345 is blocking vacuum",
		SignalIDs:       []string{"vacuum_blocked"},
		AffectedObjects: []string{"public.orders"},
		RecommendedSQL:  "SELECT pg_terminate_backend(12345)",
		ActionRisk:      "high_risk",
		Source:          "rca",
		Confidence:      0.91,
		OccurrenceCount: 3,
		DetectedAt:      time.Now().UTC().Add(-time.Hour),
		LastDetectedAt:  time.Now().UTC(),
		CausalChain: []IncidentChainLink{{
			Order:       1,
			Signal:      "vacuum_blocked",
			Description: "vacuum blocked by idle transaction",
			Evidence:    "blocker pid 12345",
		}},
	}
}

func assertCandidate(
	t *testing.T,
	got ActionCandidate,
	actionType string,
	riskTier string,
	sql string,
) {
	t.Helper()
	if got.ActionType != actionType {
		t.Fatalf("ActionType = %q, want %q", got.ActionType, actionType)
	}
	if got.RiskTier != riskTier {
		t.Fatalf("RiskTier = %q, want %q", got.RiskTier, riskTier)
	}
	if sql != "" && got.ProposedSQL != sql {
		t.Fatalf("ProposedSQL = %q, want %q", got.ProposedSQL, sql)
	}
}
