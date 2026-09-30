package value

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMergeResultsSeparatesRealizedPotentialAndIncidents(t *testing.T) {
	snapshot := Snapshot{
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
	}

	report := mergeResults([]SourceResult{{Name: "orders", Snapshot: snapshot}})

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
	if len(report.ByDatabase) != 1 || report.ByDatabase[0].Hours != 6 ||
		report.ByDatabase[0].Name != "orders" {
		t.Fatalf("ByDatabase = %#v", report.ByDatabase)
	}
	if len(report.TrendDaily) != 1 || report.TrendDaily[0].Hours != 2 {
		t.Fatalf("TrendDaily = %#v", report.TrendDaily)
	}
	if report.Partial || len(report.Unavailable) != 0 {
		t.Fatalf("complete read reported partial: %#v", report)
	}
}

// Merging sums every period, feature, day and incident across databases
// and keeps each database's own row.
func TestMergeResultsCombinesSources(t *testing.T) {
	early := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(48 * time.Hour)
	report := mergeResults([]SourceResult{
		{Name: "b", Snapshot: Snapshot{
			AllTimeMinutes: 30, MonthMinutes: 30, WeekMinutes: 0,
			ByFeatureMinutes:  map[string]float64{"index": 30},
			ByDatabaseMinutes: []DatabaseMinutes{{Name: "b", Minutes: 30}},
			PotentialMinutes:  60, IncidentMinutes: 60,
			Incidents:    []Incident{{Kind: "late", EvidenceID: "l", OccurredAt: late}},
			TrendMinutes: []DayMinutes{{Day: "2026-07-01", Minutes: 30}},
		}},
		{Name: "a", Snapshot: Snapshot{
			AllTimeMinutes: 15, MonthMinutes: 15, WeekMinutes: 15,
			ByFeatureMinutes:  map[string]float64{"index": 15, "vacuum": 6},
			ByDatabaseMinutes: []DatabaseMinutes{{Name: "a", Minutes: 15}},
			Incidents:         []Incident{{Kind: "early", EvidenceID: "e", OccurredAt: early}},
			TrendMinutes: []DayMinutes{
				{Day: "2026-07-02", Minutes: 6}, {Day: "2026-07-01", Minutes: 9},
			},
		}},
	})
	if report.DBAHoursSaved != (PeriodHours{AllTime: 0.75, ThisMonth: 0.75, ThisWeek: 0.25}) {
		t.Fatalf("periods = %#v", report.DBAHoursSaved)
	}
	if report.ByFeature["index"] != 0.75 || report.ByFeature["vacuum"] != 0.1 {
		t.Fatalf("features = %#v", report.ByFeature)
	}
	wantDB := []DatabaseHours{{Name: "b", Hours: 0.5}, {Name: "a", Hours: 0.25}}
	if !reflect.DeepEqual(report.ByDatabase, wantDB) {
		t.Fatalf("databases = %#v", report.ByDatabase)
	}
	wantTrend := []DayHours{{Day: "2026-07-01", Hours: 0.65}, {Day: "2026-07-02", Hours: 0.1}}
	if !reflect.DeepEqual(report.TrendDaily, wantTrend) {
		t.Fatalf("trend = %#v", report.TrendDaily)
	}
	if report.PotentialHoursPending != 1 || report.IncidentsAvoided.Count != 2 ||
		report.IncidentsAvoided.Detail[0].Kind != "early" {
		t.Fatalf("potential/incidents = %v %#v",
			report.PotentialHoursPending, report.IncidentsAvoided)
	}
}

// A failed database is named, flagged partial and contributes nothing;
// the other databases still count (T8 at the merge boundary).
func TestMergeResultsMarksFailedSourcePartial(t *testing.T) {
	failure := errors.New("connection refused")
	report := mergeResults([]SourceResult{
		{Name: "a", Snapshot: Snapshot{
			AllTimeMinutes:    15,
			ByDatabaseMinutes: []DatabaseMinutes{{Name: "a", Minutes: 15}},
		}},
		{Name: "c", Err: failure},
		{Name: "b", Err: failure, Snapshot: Snapshot{AllTimeMinutes: 99}},
	})
	if !report.Partial || !reflect.DeepEqual(report.Unavailable, []string{"b", "c"}) {
		t.Fatalf("partial=%v unavailable=%v", report.Partial, report.Unavailable)
	}
	if report.DBAHoursSaved.AllTime != 0.25 || len(report.ByDatabase) != 1 {
		t.Fatalf("failed source leaked value: %#v", report)
	}
}

func TestNewServiceNilRepositoryFailsClosed(t *testing.T) {
	service := NewService(nil)

	_, creditErr := service.CreditVerifiedAction(context.Background(), 1)
	if !errors.Is(creditErr, ErrRepositoryUnavailable) {
		t.Fatalf("Credit error = %v", creditErr)
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

type fakeRepository struct {
	candidate       CreditCandidate
	candidateErr    error
	stampErr        error
	stampedActionID int64
	stampedMinutes  float64
	stampedVersion  int
	stampCalls      int
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

func (f *fakeRepository) IncidentCandidate(
	context.Context, int64,
) (IncidentCandidate, error) {
	return IncidentCandidate{}, errors.New("fakeRepository has no incident graph")
}

func (f *fakeRepository) RecordIncident(
	context.Context, IncidentCredit,
) (IncidentRecord, error) {
	return IncidentRecord{}, errors.New("fakeRepository records no incidents")
}
