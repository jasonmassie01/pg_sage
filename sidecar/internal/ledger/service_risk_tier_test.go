package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// policy-01: sage.decision CHECKs risk_tier IN (read_only, safe, moderate,
// high). A tier outside that set is rejected before SQL runs, so it never
// writes an ERROR line into the customer's PostgreSQL log.

func TestRecordDecisionRejectsUnknownRiskTier(t *testing.T) {
	for _, tier := range []string{"", "unknown", "HIGH", " high", "high ", "critical"} {
		t.Run(tier, func(t *testing.T) {
			repo := &fakeRepository{insertID: 1}
			input := validDecisionInput()
			input.RiskTier = tier

			_, err := NewService(repo).RecordDecision(context.Background(), input)
			if !errors.Is(err, ErrInvalidRiskTier) {
				t.Fatalf("error = %v, want ErrInvalidRiskTier", err)
			}
			if !strings.Contains(err.Error(), "risk_tier") {
				t.Fatalf("error = %q, want it to name risk_tier", err)
			}
			if repo.insertCalls != 0 {
				t.Fatalf("insert calls = %d, want 0 (rejected before SQL)", repo.insertCalls)
			}
		})
	}
}

// Boundary: every tier the CHECK allows is accepted and persisted as is.
func TestRecordDecisionAcceptsEveryCheckedRiskTier(t *testing.T) {
	for _, tier := range []string{"read_only", "safe", "moderate", "high"} {
		t.Run(tier, func(t *testing.T) {
			repo := &fakeRepository{insertID: 9}
			input := validDecisionInput()
			input.RiskTier = tier

			decision, err := NewService(repo).RecordDecision(context.Background(), input)
			if err != nil {
				t.Fatalf("RecordDecision(%q): %v", tier, err)
			}
			if decision.ID != 9 || repo.inserted.RiskTier != tier || repo.insertCalls != 1 {
				t.Fatalf("decision=%#v inserted=%#v", decision, repo.inserted)
			}
		})
	}
}

// fakeUpserter is a FingerprintRepository that counts upserts.
type fakeUpserter struct {
	fakeRepository
	upserts int
}

func (f *fakeUpserter) UpsertDecision(
	_ context.Context, input DecisionInput,
) (int64, string, error) {
	f.upserts++
	return 5, input.EvidenceID, nil
}

// The fingerprinted (upsert) path validates the tier as well.
func TestRecordDecisionRejectsUnknownRiskTierBeforeUpsert(t *testing.T) {
	repo := &fakeUpserter{}
	input := validDecisionInput()
	input.Fingerprint = DecisionFingerprint(input)
	input.RiskTier = ""

	_, err := NewService(repo).RecordDecision(context.Background(), input)
	if !errors.Is(err, ErrInvalidRiskTier) || repo.upserts != 0 || repo.insertCalls != 0 {
		t.Fatalf("err=%v upserts=%d inserts=%d, want ErrInvalidRiskTier and no SQL",
			err, repo.upserts, repo.insertCalls)
	}
	input.RiskTier = "high"
	decision, err := NewService(repo).RecordDecision(context.Background(), input)
	if err != nil || decision.ID != 5 || repo.upserts != 1 {
		t.Fatalf("decision=%#v err=%v upserts=%d, want one upsert", decision, err, repo.upserts)
	}
}
