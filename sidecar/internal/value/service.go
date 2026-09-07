package value

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrRepositoryUnavailable = errors.New("value repository unavailable")
	ErrCreditNotEligible     = errors.New("action is not eligible for toil credit")
	ErrToilModelUnavailable  = errors.New("toil model unavailable")
)

type Repository interface {
	ReadSnapshot(context.Context, Filter) (Snapshot, error)
	CreditCandidate(context.Context, int64) (CreditCandidate, error)
	StampCredit(context.Context, int64, float64, int) error
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Get(ctx context.Context, filter Filter) (Report, error) {
	if s == nil || s.repository == nil {
		return Report{}, ErrRepositoryUnavailable
	}
	snapshot, err := s.repository.ReadSnapshot(ctx, filter)
	if err != nil {
		return Report{}, fmt.Errorf("read value snapshot: %w", err)
	}
	return reportFromSnapshot(snapshot), nil
}

func (s *Service) CreditVerifiedAction(
	ctx context.Context, actionID int64,
) (Credit, error) {
	if s == nil || s.repository == nil {
		return Credit{}, ErrRepositoryUnavailable
	}
	candidate, err := s.repository.CreditCandidate(ctx, actionID)
	if err != nil {
		return Credit{}, fmt.Errorf("load credit candidate: %w", err)
	}
	if candidate.Outcome != "success" ||
		candidate.VerificationState != "verified" {
		return Credit{}, ErrCreditNotEligible
	}
	if candidate.ModelMinutes <= 0 || candidate.ModelVersion <= 0 {
		return Credit{}, ErrToilModelUnavailable
	}
	if err := s.repository.StampCredit(
		ctx, actionID, candidate.ModelMinutes, candidate.ModelVersion,
	); err != nil {
		return Credit{}, fmt.Errorf("stamp verified toil credit: %w", err)
	}
	return Credit{
		ActionID: actionID, Minutes: candidate.ModelMinutes,
		ModelVersion: candidate.ModelVersion,
	}, nil
}

func reportFromSnapshot(snapshot Snapshot) Report {
	features := make(map[string]float64, len(snapshot.ByFeatureMinutes))
	for feature, minutes := range snapshot.ByFeatureMinutes {
		features[feature] = hours(minutes)
	}
	databases := make([]DatabaseHours, 0, len(snapshot.ByDatabaseMinutes))
	for _, row := range snapshot.ByDatabaseMinutes {
		databases = append(databases, DatabaseHours{
			Name: row.Name, Hours: hours(row.Minutes),
		})
	}
	trend := make([]DayHours, 0, len(snapshot.TrendMinutes))
	for _, row := range snapshot.TrendMinutes {
		trend = append(trend, DayHours{Day: row.Day, Hours: hours(row.Minutes)})
	}
	incidents := append([]Incident(nil), snapshot.Incidents...)
	return Report{
		DBAHoursSaved: PeriodHours{
			AllTime:   hours(snapshot.AllTimeMinutes),
			ThisMonth: hours(snapshot.MonthMinutes),
			ThisWeek:  hours(snapshot.WeekMinutes),
		},
		ByFeature:  features,
		ByDatabase: databases,
		IncidentsAvoided: IncidentSummary{
			Count: len(incidents), CreditedHours: hours(snapshot.IncidentMinutes),
			Detail: incidents,
		},
		PotentialHoursPending: hours(snapshot.PotentialMinutes),
		TrendDaily:            trend,
	}
}

func hours(minutes float64) float64 { return minutes / 60 }
