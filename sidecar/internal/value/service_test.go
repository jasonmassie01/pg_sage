package value

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestServiceGetSeparatesRealizedPotentialAndIncidents(t *testing.T) {
	repo := &fakeRepository{snapshot: Snapshot{
		AllTimeMinutes: 600,
		MonthMinutes:   180,
		WeekMinutes:    60,
		ByFeatureMinutes: map[string]float64{
			"index": 270,
			"wal":   90,
		},
		ByDatabaseMinutes: []DatabaseMinutes{{Name: "orders", Minutes: 360}},
		PotentialMinutes:  240,
		IncidentMinutes:   480,
		Incidents: []Incident{{
			Kind:       "xid_wraparound",
			Severity:   "prevented",
			EvidenceID: "ev-7",
			OccurredAt: time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC),
		}},
		TrendMinutes: []DayMinutes{{Day: "2026-07-20", Minutes: 120}},
	}}

	report, err := NewService(repo).Get(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if report.DBAHoursSaved.AllTime != 10 ||
		report.DBAHoursSaved.ThisMonth != 3 ||
		report.DBAHoursSaved.ThisWeek != 1 {
		t.Fatalf("DBAHoursSaved = %#v", report.DBAHoursSaved)
	}
	if report.PotentialHoursPending != 4 {
		t.Fatalf("PotentialHoursPending = %v, want 4", report.PotentialHoursPending)
	}
	if report.IncidentsAvoided.Count != 1 ||
		report.IncidentsAvoided.CreditedHours != 8 {
		t.Fatalf("IncidentsAvoided = %#v", report.IncidentsAvoided)
	}
	if report.ByFeature["index"] != 4.5 || report.ByFeature["wal"] != 1.5 {
		t.Fatalf("ByFeature = %#v", report.ByFeature)
	}
	if len(report.ByDatabase) != 1 || report.ByDatabase[0].Hours != 6 {
		t.Fatalf("ByDatabase = %#v", report.ByDatabase)
	}
	if len(report.TrendDaily) != 1 || report.TrendDaily[0].Hours != 2 {
		t.Fatalf("TrendDaily = %#v", report.TrendDaily)
	}
}

func TestServiceGetForwardsDatabaseAndTimeFilter(t *testing.T) {
	repo := &fakeRepository{}
	filter := Filter{
		Database: "orders",
		Since:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}

	_, err := NewService(repo).Get(context.Background(), filter)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if repo.lastFilter != filter {
		t.Fatalf("filter = %#v, want %#v", repo.lastFilter, filter)
	}
}

func TestServiceGetPropagatesRepositoryError(t *testing.T) {
	repo := &fakeRepository{snapshotErr: errors.New("database unavailable")}

	_, err := NewService(repo).Get(context.Background(), Filter{})
	if err == nil || !errors.Is(err, repo.snapshotErr) {
		t.Fatalf("Get error = %v, want wrapped repository error", err)
	}
}

func TestCreditVerifiedActionStampsVersionedToilOnlyAfterVerification(t *testing.T) {
	repo := &fakeRepository{candidate: CreditCandidate{
		ActionID:          42,
		ActionType:        "create_index_concurrently",
		Outcome:           "success",
		VerificationState: "verified",
		ModelMinutes:      45,
		ModelVersion:      1,
	}}

	credit, err := NewService(repo).CreditVerifiedAction(context.Background(), 42)
	if err != nil {
		t.Fatalf("CreditVerifiedAction: %v", err)
	}
	if credit.Minutes != 45 || credit.ModelVersion != 1 {
		t.Fatalf("credit = %#v", credit)
	}
	if repo.stampedActionID != 42 || repo.stampedMinutes != 45 ||
		repo.stampedVersion != 1 {
		t.Fatalf("stamp = id %d minutes %v version %d",
			repo.stampedActionID, repo.stampedMinutes, repo.stampedVersion)
	}
}

func TestCreditVerifiedActionRejectsUnverifiedRevertedAndFailed(t *testing.T) {
	tests := []CreditCandidate{
		{ActionID: 1, Outcome: "pending", VerificationState: "verified"},
		{ActionID: 2, Outcome: "success", VerificationState: "pending"},
		{ActionID: 3, Outcome: "reverted", VerificationState: "verified"},
		{ActionID: 4, Outcome: "failed", VerificationState: "verified"},
	}
	for _, candidate := range tests {
		t.Run(candidate.Outcome+candidate.VerificationState, func(t *testing.T) {
			repo := &fakeRepository{candidate: candidate}

			credit, err := NewService(repo).CreditVerifiedAction(
				context.Background(), candidate.ActionID,
			)
			if !errors.Is(err, ErrCreditNotEligible) {
				t.Fatalf("error = %v, want ErrCreditNotEligible", err)
			}
			if credit.Minutes != 0 || repo.stampCalls != 0 {
				t.Fatalf("credit=%#v stampCalls=%d, want zero", credit, repo.stampCalls)
			}
		})
	}
}

func TestCreditVerifiedActionRejectsMissingOrInvalidModel(t *testing.T) {
	tests := []CreditCandidate{
		{
			ActionID: 1, Outcome: "success", VerificationState: "verified",
			ModelMinutes: 0, ModelVersion: 1,
		},
		{
			ActionID: 2, Outcome: "success", VerificationState: "verified",
			ModelMinutes: 45, ModelVersion: 0,
		},
	}
	for _, candidate := range tests {
		repo := &fakeRepository{candidate: candidate}

		_, err := NewService(repo).CreditVerifiedAction(
			context.Background(), candidate.ActionID,
		)
		if !errors.Is(err, ErrToilModelUnavailable) {
			t.Fatalf("candidate %#v error = %v", candidate, err)
		}
		if repo.stampCalls != 0 {
			t.Fatalf("stamp calls = %d, want 0", repo.stampCalls)
		}
	}
}

func TestCreditVerifiedActionPropagatesStampFailure(t *testing.T) {
	repo := &fakeRepository{
		candidate: CreditCandidate{
			ActionID: 9, Outcome: "success", VerificationState: "verified",
			ModelMinutes: 20, ModelVersion: 2,
		},
		stampErr: errors.New("write failed"),
	}

	_, err := NewService(repo).CreditVerifiedAction(context.Background(), 9)
	if err == nil || !errors.Is(err, repo.stampErr) {
		t.Fatalf("error = %v, want wrapped stamp error", err)
	}
}

func TestNewServiceNilRepositoryFailsClosed(t *testing.T) {
	service := NewService(nil)

	_, getErr := service.Get(context.Background(), Filter{})
	if !errors.Is(getErr, ErrRepositoryUnavailable) {
		t.Fatalf("Get error = %v", getErr)
	}
	_, creditErr := service.CreditVerifiedAction(context.Background(), 1)
	if !errors.Is(creditErr, ErrRepositoryUnavailable) {
		t.Fatalf("Credit error = %v", creditErr)
	}
}

type fakeRepository struct {
	snapshot        Snapshot
	snapshotErr     error
	candidate       CreditCandidate
	candidateErr    error
	stampErr        error
	lastFilter      Filter
	stampedActionID int64
	stampedMinutes  float64
	stampedVersion  int
	stampCalls      int
}

func (f *fakeRepository) ReadSnapshot(
	_ context.Context, filter Filter,
) (Snapshot, error) {
	f.lastFilter = filter
	return f.snapshot, f.snapshotErr
}

func (f *fakeRepository) CreditCandidate(
	_ context.Context, actionID int64,
) (CreditCandidate, error) {
	if f.candidate.ActionID == 0 {
		f.candidate.ActionID = actionID
	}
	return f.candidate, f.candidateErr
}

func (f *fakeRepository) StampCredit(
	_ context.Context, actionID int64, minutes float64, version int,
) error {
	f.stampCalls++
	f.stampedActionID = actionID
	f.stampedMinutes = minutes
	f.stampedVersion = version
	return f.stampErr
}
