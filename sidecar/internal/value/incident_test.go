package value

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var incidentAt = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func verifiedIncidentCandidate() IncidentCandidate {
	databaseID := int64(7)
	return IncidentCandidate{
		ActionID: 42, DatabaseID: &databaseID, DecisionID: 11, VerificationID: 13,
		Outcome: "success", VerificationState: "verified",
	}
}

func TestCreditAvoidedIncidentRecordsConservativeNearMiss(t *testing.T) {
	repo := &fakeIncidentRepository{candidate: verifiedIncidentCandidate()}

	record, err := NewService(repo).CreditAvoidedIncident(context.Background(),
		AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
			Severity: SeverityNearMiss, OccurredAt: incidentAt})
	if err != nil {
		t.Fatalf("CreditAvoidedIncident: %v", err)
	}
	if record.ID != 99 || record.CreditedMinutes != 120 {
		t.Fatalf("record = %#v, want id 99 and 120 minutes", record)
	}
	got := repo.recorded
	if repo.recordCalls != 1 || got.Kind != "xid_wraparound" ||
		got.Severity != "near_miss" || got.CreditedMinutes != 120 ||
		got.EvidenceID != "incident:xid_wraparound:action:42" ||
		got.ModelVersion != 1 || got.DecisionID != 11 || got.ActionLogID != 42 ||
		got.VerificationID != 13 || !got.OccurredAt.Equal(incidentAt) {
		t.Fatalf("recorded = %#v (calls %d)", got, repo.recordCalls)
	}
	if got.DatabaseID == nil || *got.DatabaseID != 7 {
		t.Fatalf("database id = %v, want 7", got.DatabaseID)
	}
}

func TestCreditAvoidedIncidentMinutesBySeverity(t *testing.T) {
	for severity, want := range map[string]float64{
		SeverityNearMiss: 120, SeverityPrevented: 480,
	} {
		repo := &fakeIncidentRepository{candidate: verifiedIncidentCandidate()}
		_, err := NewService(repo).CreditAvoidedIncident(context.Background(),
			AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
				Severity: severity, OccurredAt: incidentAt})
		if err != nil || repo.recorded.CreditedMinutes != want {
			t.Fatalf("%s minutes = %v (%v), want %v",
				severity, repo.recorded.CreditedMinutes, err, want)
		}
	}
}

func TestCreditAvoidedIncidentKeepsNullDatabase(t *testing.T) {
	candidate := verifiedIncidentCandidate()
	candidate.DatabaseID = nil
	repo := &fakeIncidentRepository{candidate: candidate}
	_, err := NewService(repo).CreditAvoidedIncident(context.Background(),
		AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
			Severity: SeverityNearMiss, OccurredAt: incidentAt})
	if err != nil || repo.recordCalls != 1 || repo.recorded.DatabaseID != nil {
		t.Fatalf("recorded = %#v calls=%d err=%v", repo.recorded, repo.recordCalls, err)
	}
}

func TestCreditAvoidedIncidentRejectsInvalidInput(t *testing.T) {
	valid := AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
		Severity: SeverityNearMiss, OccurredAt: incidentAt}
	tests := map[string]func(*AvoidedIncident){
		"zero action":        func(i *AvoidedIncident) { i.ActionID = 0 },
		"negative action":    func(i *AvoidedIncident) { i.ActionID = -5 },
		"empty kind":         func(i *AvoidedIncident) { i.Kind = "" },
		"unknown kind":       func(i *AvoidedIncident) { i.Kind = "disk_melt" },
		"kind with sql":      func(i *AvoidedIncident) { i.Kind = "x'; DROP TABLE t;--" },
		"empty severity":     func(i *AvoidedIncident) { i.Severity = "" },
		"unknown severity":   func(i *AvoidedIncident) { i.Severity = "catastrophic" },
		"zero occurred time": func(i *AvoidedIncident) { i.OccurredAt = time.Time{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			incident := valid
			mutate(&incident)
			repo := &fakeIncidentRepository{candidate: verifiedIncidentCandidate()}
			record, err := NewService(repo).CreditAvoidedIncident(
				context.Background(), incident)
			if !errors.Is(err, ErrInvalidIncident) {
				t.Fatalf("error = %v, want ErrInvalidIncident", err)
			}
			if record != (IncidentRecord{}) || repo.candidateCalls != 0 ||
				repo.recordCalls != 0 {
				t.Fatalf("record=%#v candidateCalls=%d recordCalls=%d, want none",
					record, repo.candidateCalls, repo.recordCalls)
			}
		})
	}
}

// Every known kind the schema accepts is accepted by the service, so the
// two lists cannot drift apart silently.
func TestCreditAvoidedIncidentAcceptsEverySchemaKind(t *testing.T) {
	for _, kind := range []string{"xid_wraparound", "disk_full_slot", "lock_storm"} {
		repo := &fakeIncidentRepository{candidate: verifiedIncidentCandidate()}
		_, err := NewService(repo).CreditAvoidedIncident(context.Background(),
			AvoidedIncident{ActionID: 42, Kind: kind, Severity: SeverityNearMiss,
				OccurredAt: incidentAt})
		if err != nil || repo.recorded.EvidenceID != "incident:"+kind+":action:42" {
			t.Fatalf("kind %s: evidence=%q err=%v", kind, repo.recorded.EvidenceID, err)
		}
	}
}

func TestCreditAvoidedIncidentRequiresVerifiedSuccess(t *testing.T) {
	tests := map[string]func(*IncidentCandidate){
		"failed action":        func(c *IncidentCandidate) { c.Outcome = "failed" },
		"monitoring action":    func(c *IncidentCandidate) { c.Outcome = "monitoring" },
		"rolled back action":   func(c *IncidentCandidate) { c.Outcome = "rolled_back" },
		"pending verification": func(c *IncidentCandidate) { c.VerificationState = "pending" },
		"missing verification": func(c *IncidentCandidate) { c.VerificationState = "missing" },
		"unverifiable":         func(c *IncidentCandidate) { c.VerificationState = "unverifiable" },
		"no decision":          func(c *IncidentCandidate) { c.DecisionID = 0 },
		"no verification id":   func(c *IncidentCandidate) { c.VerificationID = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := verifiedIncidentCandidate()
			mutate(&candidate)
			repo := &fakeIncidentRepository{candidate: candidate}
			record, err := NewService(repo).CreditAvoidedIncident(context.Background(),
				AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
					Severity: SeverityNearMiss, OccurredAt: incidentAt})
			if !errors.Is(err, ErrCreditNotEligible) {
				t.Fatalf("error = %v, want ErrCreditNotEligible", err)
			}
			if record != (IncidentRecord{}) || repo.recordCalls != 0 {
				t.Fatalf("record=%#v recordCalls=%d, want none", record, repo.recordCalls)
			}
		})
	}
}

func TestCreditAvoidedIncidentPropagatesRepositoryErrors(t *testing.T) {
	incident := AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
		Severity: SeverityNearMiss, OccurredAt: incidentAt}
	candidateErr := errors.New("connection refused")
	repo := &fakeIncidentRepository{candidateErr: candidateErr}
	_, err := NewService(repo).CreditAvoidedIncident(context.Background(), incident)
	if !errors.Is(err, candidateErr) || !strings.Contains(err.Error(), "incident candidate") ||
		repo.recordCalls != 0 {
		t.Fatalf("candidate error = %v (record calls %d)", err, repo.recordCalls)
	}

	recordErr := errors.New("permission denied for table incident_avoided")
	repo = &fakeIncidentRepository{candidate: verifiedIncidentCandidate(), recordErr: recordErr}
	_, err = NewService(repo).CreditAvoidedIncident(context.Background(), incident)
	if !errors.Is(err, recordErr) || !strings.Contains(err.Error(), "record avoided incident") {
		t.Fatalf("record error = %v", err)
	}

	repo = &fakeIncidentRepository{candidate: verifiedIncidentCandidate(),
		recordErr: ErrIncidentAlreadyCredited}
	_, err = NewService(repo).CreditAvoidedIncident(context.Background(), incident)
	if !errors.Is(err, ErrIncidentAlreadyCredited) {
		t.Fatalf("duplicate error = %v, want ErrIncidentAlreadyCredited", err)
	}
}

func TestCreditAvoidedIncidentNilServiceFailsClosed(t *testing.T) {
	incident := AvoidedIncident{ActionID: 42, Kind: IncidentXIDWraparound,
		Severity: SeverityNearMiss, OccurredAt: incidentAt}
	var nilService *Service
	if _, err := nilService.CreditAvoidedIncident(context.Background(), incident); !errors.Is(
		err, ErrRepositoryUnavailable) {
		t.Fatalf("nil service error = %v", err)
	}
	if _, err := NewService(nil).CreditAvoidedIncident(context.Background(), incident); !errors.Is(
		err, ErrRepositoryUnavailable) {
		t.Fatalf("nil repository error = %v", err)
	}
}

// No concurrent access tests here: the service holds no state. Concurrent
// crediting is covered against Postgres in incident_postgres_test.go.

type fakeIncidentRepository struct {
	fakeRepository
	candidate      IncidentCandidate
	candidateErr   error
	candidateCalls int
	recordErr      error
	recorded       IncidentCredit
	recordCalls    int
}

func (f *fakeIncidentRepository) IncidentCandidate(
	_ context.Context, _ int64,
) (IncidentCandidate, error) {
	f.candidateCalls++
	return f.candidate, f.candidateErr
}

func (f *fakeIncidentRepository) RecordIncident(
	_ context.Context, input IncidentCredit,
) (IncidentRecord, error) {
	f.recordCalls++
	f.recorded = input
	if f.recordErr != nil {
		return IncidentRecord{}, f.recordErr
	}
	return IncidentRecord{ID: 99, CreditedMinutes: input.CreditedMinutes}, nil
}
