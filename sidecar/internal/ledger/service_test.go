package ledger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordDecisionAssignsEvidenceAndPersistsSafetyContext(t *testing.T) {
	repo := &fakeRepository{insertID: 17}
	service := NewService(repo)
	input := validDecisionInput()

	decision, err := service.RecordDecision(context.Background(), input)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if decision.ID != 17 {
		t.Fatalf("ID = %d, want 17", decision.ID)
	}
	if !strings.HasPrefix(decision.EvidenceID, "ev_") {
		t.Fatalf("EvidenceID = %q", decision.EvidenceID)
	}
	if repo.inserted.PolicyVersion != 3 ||
		repo.inserted.Verdict != VerdictPark ||
		repo.inserted.Reason != "outside_window" {
		t.Fatalf("inserted = %#v", repo.inserted)
	}
	if repo.inserted.EvidenceID != decision.EvidenceID {
		t.Fatalf("inserted evidence = %q, decision = %q",
			repo.inserted.EvidenceID, decision.EvidenceID)
	}
}

func TestRecordDecisionPreservesCallerCorrelationID(t *testing.T) {
	repo := &fakeRepository{insertID: 2}
	input := validDecisionInput()
	input.EvidenceID = "ev_existing_42"

	decision, err := NewService(repo).RecordDecision(context.Background(), input)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if decision.EvidenceID != input.EvidenceID {
		t.Fatalf("EvidenceID = %q, want %q", decision.EvidenceID, input.EvidenceID)
	}
}

func TestRecordDecisionRejectsIncompleteOrUnknownDecisions(t *testing.T) {
	tests := []struct {
		name string
		edit func(*DecisionInput)
		want string
	}{
		{"missing feature", func(in *DecisionInput) { in.Feature = "" }, "feature"},
		{"missing intent", func(in *DecisionInput) { in.Intent = "" }, "intent"},
		{"missing reason", func(in *DecisionInput) { in.Reason = "" }, "reason"},
		{
			"unknown verdict",
			func(in *DecisionInput) { in.Verdict = Verdict("maybe") },
			"verdict",
		},
		{
			"missing policy version",
			func(in *DecisionInput) { in.PolicyVersion = 0 },
			"policy_version",
		},
		{
			"deadline missing hard time",
			func(in *DecisionInput) {
				in.DeadlineKind = "xid"
				in.DeadlineHardAt = nil
			},
			"deadline",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeRepository{}
			input := validDecisionInput()
			test.edit(&input)

			_, err := NewService(repo).RecordDecision(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if repo.insertCalls != 0 {
				t.Fatalf("insert calls = %d, want 0", repo.insertCalls)
			}
		})
	}
}

func TestRecordDecisionPropagatesRepositoryFailure(t *testing.T) {
	repo := &fakeRepository{insertErr: errors.New("connection lost")}

	_, err := NewService(repo).RecordDecision(
		context.Background(), validDecisionInput(),
	)
	if err == nil || !errors.Is(err, repo.insertErr) {
		t.Fatalf("error = %v, want wrapped repository error", err)
	}
}

func TestSelfAuditReturnsEveryUngatedMutation(t *testing.T) {
	repo := &fakeRepository{violations: []AuditViolation{
		{ActionID: 10, Kind: "missing_decision"},
		{ActionID: 11, Kind: "missing_verification"},
	}}

	result, err := NewService(repo).SelfAudit(context.Background())
	if err != nil {
		t.Fatalf("SelfAudit: %v", err)
	}
	if result.OK || result.Count != 2 || len(result.Violations) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if result.Violations[0].ActionID != 10 ||
		result.Violations[1].Kind != "missing_verification" {
		t.Fatalf("violations = %#v", result.Violations)
	}
}

func TestSelfAuditReportsCleanLedger(t *testing.T) {
	result, err := NewService(&fakeRepository{}).SelfAudit(context.Background())
	if err != nil {
		t.Fatalf("SelfAudit: %v", err)
	}
	if !result.OK || result.Count != 0 || len(result.Violations) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestEvidenceIDsAreUniqueUnderConcurrentWriters(t *testing.T) {
	const count = 128
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- NewEvidenceID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, count)
	for id := range ids {
		if !strings.HasPrefix(id, "ev_") {
			t.Fatalf("ID = %q, want ev_ prefix", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate evidence ID %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("unique IDs = %d, want %d", len(seen), count)
	}
}

func TestNilRepositoryFailsClosed(t *testing.T) {
	service := NewService(nil)

	_, recordErr := service.RecordDecision(
		context.Background(), validDecisionInput(),
	)
	if !errors.Is(recordErr, ErrRepositoryUnavailable) {
		t.Fatalf("RecordDecision error = %v", recordErr)
	}
	_, auditErr := service.SelfAudit(context.Background())
	if !errors.Is(auditErr, ErrRepositoryUnavailable) {
		t.Fatalf("SelfAudit error = %v", auditErr)
	}
}

func validDecisionInput() DecisionInput {
	hardAt := time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC)
	return DecisionInput{
		DatabaseID:     intPointer(7),
		Feature:        "index",
		Intent:         "create supporting index",
		Evidence:       map[string]any{"query_id": int64(42)},
		ProposedSQL:    "CREATE INDEX CONCURRENTLY idx ON public.orders (status)",
		Verdict:        VerdictPark,
		Reason:         "outside_window",
		RiskTier:       "safe",
		PolicyVersion:  3,
		TargetObjects:  []string{"public.orders"},
		DeadlineKind:   "disk",
		DeadlineHardAt: &hardAt,
	}
}

func intPointer(value int) *int { return &value }

type fakeRepository struct {
	insertID    int64
	insertErr   error
	inserted    DecisionInput
	insertCalls int
	violations  []AuditViolation
	auditErr    error
}

func (f *fakeRepository) InsertDecision(
	_ context.Context, input DecisionInput,
) (int64, error) {
	f.insertCalls++
	f.inserted = input
	return f.insertID, f.insertErr
}

func (f *fakeRepository) FindAuditViolations(
	context.Context,
) ([]AuditViolation, error) {
	return append([]AuditViolation(nil), f.violations...), f.auditErr
}
