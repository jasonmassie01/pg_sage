package cases

import (
	"testing"
	"time"
)

var evidenceAnchor = time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

func expiryOffsets(t *testing.T, c Case, anchor time.Time) []time.Duration {
	t.Helper()
	if len(c.ActionCandidates) == 0 {
		t.Fatal("no action candidates projected")
	}
	out := make([]time.Duration, 0, len(c.ActionCandidates))
	for _, candidate := range c.ActionCandidates {
		if candidate.ExpiresAt == nil {
			t.Fatalf("%s has no expiry", candidate.ActionType)
		}
		out = append(out, candidate.ExpiresAt.Sub(anchor))
	}
	return out
}

// G2-B24: incident candidates expired relative to time.Now() on every read,
// so a stale incident's cancel/terminate proposal never expired.
func TestIncidentCandidateExpiryAnchoredToLastDetection(t *testing.T) {
	incident := testIncident("prod")
	incident.DetectedAt = evidenceAnchor.Add(-time.Hour)
	incident.LastDetectedAt = evidenceAnchor

	got := ProjectIncident(incident)

	for i, offset := range expiryOffsets(t, got, evidenceAnchor) {
		if offset != 15*time.Minute {
			t.Errorf("candidate %d expiry = anchor%+v, want anchor+15m", i, offset)
		}
		if got.ActionCandidates[i].IsExecutable(time.Now()) {
			t.Errorf("candidate %d from a days-old incident is still executable", i)
		}
	}
	if !got.ObservedAt.Equal(evidenceAnchor) {
		t.Fatalf("ObservedAt = %v, want last detection %v", got.ObservedAt, evidenceAnchor)
	}
}

func TestIncidentCandidateExpiryStableAcrossReads(t *testing.T) {
	incident := SourceIncident{
		ID: "inc-av", DatabaseName: "prod", Severity: SeverityWarning,
		RootCause:       "Autovacuum is falling behind on public.orders",
		SignalIDs:       []string{"autovacuum_falling_behind"},
		AffectedObjects: []string{"public.orders"},
		LastDetectedAt:  evidenceAnchor,
	}
	first := ProjectIncident(incident)
	time.Sleep(5 * time.Millisecond)
	second := ProjectIncident(incident)
	for i := range first.ActionCandidates {
		a, b := first.ActionCandidates[i].ExpiresAt, second.ActionCandidates[i].ExpiresAt
		if !a.Equal(*b) {
			t.Fatalf("candidate %d expiry moved between reads: %v -> %v", i, a, b)
		}
	}
	if offsets := expiryOffsets(t, first, evidenceAnchor); offsets[1] != 24*time.Hour {
		t.Fatalf("vacuum candidate expiry offset = %v, want 24h", offsets[1])
	}
}

// Only DetectedAt known (legacy rows): it anchors the expiry.
func TestIncidentCandidateExpiryFallsBackToDetectedAt(t *testing.T) {
	incident := SourceIncident{
		ID: "inc-seq", DatabaseName: "prod", Severity: SeverityCritical,
		SignalIDs:       []string{"sequence_exhaustion"},
		AffectedObjects: []string{"public.orders_id_seq"},
		DetectedAt:      evidenceAnchor,
	}
	offsets := expiryOffsets(t, ProjectIncident(incident), evidenceAnchor)
	if offsets[0] != 24*time.Hour {
		t.Fatalf("expiry offset = %v, want DetectedAt+24h", offsets[0])
	}
}

// G2-B24: query-hint candidates are anchored to the hint's latest evidence
// (rollback/verification time, else creation), not to the request time.
func TestQueryHintCandidateExpiryAnchoredToHintEvidence(t *testing.T) {
	rolledBack := evidenceAnchor.Add(2 * time.Hour)
	broken := SourceQueryHint{
		QueryID: 42, DatabaseName: "prod", HintText: "SeqScan(t)",
		Status: "broken", CreatedAt: evidenceAnchor, RolledBackAt: &rolledBack,
	}
	if got := expiryOffsets(t, ProjectQueryHint(broken), rolledBack); got[0] != 24*time.Hour {
		t.Fatalf("retire expiry = rollback%+v, want rollback+24h", got[0])
	}

	rewrite := SourceQueryHint{
		QueryID: 43, DatabaseName: "prod", Status: "active",
		CreatedAt: evidenceAnchor, SuggestedRewrite: "SELECT 1",
	}
	if got := expiryOffsets(t, ProjectQueryHint(rewrite), evidenceAnchor); got[0] != 7*24*time.Hour {
		t.Fatalf("rewrite expiry = created%+v, want created+7d", got[0])
	}
}

// Nil/zero: with no evidence time at all the projector still yields a
// bounded, future expiry instead of a zero timestamp.
func TestCandidateExpiryWithoutEvidenceTimeIsBounded(t *testing.T) {
	before := time.Now().UTC()
	hint := SourceQueryHint{QueryID: 44, Status: "broken", HintText: "x"}
	candidate := ProjectQueryHint(hint).ActionCandidates[0]
	if candidate.ExpiresAt.Before(before.Add(24*time.Hour)) ||
		candidate.ExpiresAt.After(time.Now().UTC().Add(24*time.Hour)) {
		t.Fatalf("expiry %v not within now+24h", candidate.ExpiresAt)
	}
}
