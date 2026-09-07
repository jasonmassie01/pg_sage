package value

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) ReadSnapshot(ctx context.Context, filter Filter) (Snapshot, error) {
	if r == nil || r.pool == nil {
		return Snapshot{}, ErrRepositoryUnavailable
	}
	result := Snapshot{ByFeatureMinutes: map[string]float64{}}
	if err := r.readRealized(ctx, filter, &result); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot realized value: %w", err)
	}
	if err := r.readPotential(ctx, filter, &result); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot potential value: %w", err)
	}
	if err := r.readIncidents(ctx, filter, &result); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot incidents: %w", err)
	}
	return result, nil
}

func (r *PostgresRepository) CreditCandidate(
	ctx context.Context, actionID int64,
) (CreditCandidate, error) {
	var item CreditCandidate
	err := r.pool.QueryRow(ctx, `SELECT al.id, al.action_type, al.outcome,
		CASE WHEN v.verdict='success' AND v.completed_at IS NOT NULL
			THEN 'verified' ELSE COALESCE(v.verdict, 'missing') END,
		COALESCE(tm.base_minutes, 0), COALESCE(tm.model_version, 0)
		FROM sage.action_log al
		LEFT JOIN sage.verification v ON v.action_log_id=al.id
		LEFT JOIN sage.toil_model tm ON tm.action_type=CASE al.action_type
			WHEN 'create_index' THEN 'create_index_concurrently'
			WHEN 'drop_index' THEN 'drop_unused_index'
			WHEN 'reindex' THEN 'reindex_concurrently'
			WHEN 'vacuum' THEN 'vacuum_table'
			WHEN 'analyze' THEN 'analyze_table'
			ELSE al.action_type END
			AND tm.effective_to IS NULL WHERE al.id=$1`, actionID).Scan(
		&item.ActionID, &item.ActionType, &item.Outcome, &item.VerificationState,
		&item.ModelMinutes, &item.ModelVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditCandidate{}, fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return CreditCandidate{}, fmt.Errorf("load action credit candidate: %w", err)
	}
	return item, nil
}

func (r *PostgresRepository) StampCredit(
	ctx context.Context, actionID int64, minutes float64, modelVersion int,
) error {
	tag, err := r.pool.Exec(ctx, `UPDATE sage.action_log SET
		toil_minutes_saved=COALESCE(toil_minutes_saved, $2),
		toil_model_version=COALESCE(toil_model_version, $3)
		WHERE id=$1 AND outcome='success' AND EXISTS
		(SELECT 1 FROM sage.verification WHERE action_log_id=$1
		 AND verdict='success' AND completed_at IS NOT NULL)`,
		actionID, minutes, modelVersion)
	if err != nil {
		return fmt.Errorf("stamp action credit: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCreditNotEligible
	}
	return nil
}

func (r *PostgresRepository) ZeroCreditOnRevert(
	ctx context.Context, actionID int64, outcome string,
) (ZeroCreditResult, error) {
	return r.zeroCreditAtomic(ctx, actionID, outcome)
}

func (r *PostgresRepository) zeroCreditAtomic(
	ctx context.Context, actionID int64, outcome string,
) (ZeroCreditResult, error) {
	var previous float64
	var applied bool
	err := r.pool.QueryRow(ctx, `WITH old AS (
		SELECT id, COALESCE(toil_minutes_saved,0)::float8 AS minutes
		FROM sage.action_log WHERE id=$1 FOR UPDATE), changed AS (
		UPDATE sage.action_log al SET toil_minutes_saved=0, outcome=$2
		FROM old WHERE al.id=old.id RETURNING old.minutes)
		SELECT minutes, minutes<>0 FROM changed`, actionID, outcome).Scan(&previous, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return ZeroCreditResult{}, fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return ZeroCreditResult{}, fmt.Errorf("zero action credit: %w", err)
	}
	return ZeroCreditResult{Applied: applied, PreviousMinutes: previous}, nil
}

func (r *PostgresRepository) RecordIncident(
	ctx context.Context, input IncidentCredit,
) (IncidentRecord, error) {
	var result IncidentRecord
	err := r.pool.QueryRow(ctx, `INSERT INTO sage.incident_avoided
		(database_id, kind, severity, credited_minutes, evidence_id, model_version,
		 decision_id, action_log_id, verification_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id, credited_minutes`, input.DatabaseID, input.Kind, input.Severity,
		input.CreditedMinutes, input.EvidenceID, input.ModelVersion, input.DecisionID,
		input.ActionLogID, input.VerificationID, input.OccurredAt).Scan(
		&result.ID, &result.CreditedMinutes)
	if err != nil {
		return IncidentRecord{}, fmt.Errorf("record incident credit: %w", err)
	}
	return result, nil
}

func (r *PostgresRepository) readRealized(
	ctx context.Context, filter Filter, result *Snapshot,
) error {
	rows, err := r.pool.Query(ctx, `SELECT d.name, al.action_type,
		date_trunc('day', al.executed_at)::date::text, al.executed_at,
		al.toil_minutes_saved::float8 FROM sage.action_log al
		LEFT JOIN sage.databases d ON d.id=al.database_id
		WHERE al.outcome='success' AND al.toil_minutes_saved IS NOT NULL
		AND ($1='' OR d.name=$1) AND ($2::timestamptz IS NULL OR al.executed_at >= $2)
		AND ($3::timestamptz IS NULL OR al.executed_at <= $3)
		ORDER BY al.executed_at`, filter.Database, nullableTime(filter.Since),
		nullableTime(filter.Until))
	if err != nil {
		return err
	}
	defer rows.Close()
	byDB := map[string]float64{}
	byDay := map[string]float64{}
	now := time.Now().UTC()
	for rows.Next() {
		var db, feature, day string
		var at time.Time
		var minutes float64
		if err := rows.Scan(&db, &feature, &day, &at, &minutes); err != nil {
			return err
		}
		result.AllTimeMinutes += minutes
		if sameMonth(at, now) {
			result.MonthMinutes += minutes
		}
		if !at.Before(startOfWeek(now)) {
			result.WeekMinutes += minutes
		}
		result.ByFeatureMinutes[feature] += minutes
		byDB[db] += minutes
		byDay[day] += minutes
	}
	result.ByDatabaseMinutes = databaseRows(byDB)
	result.TrendMinutes = dayRows(byDay)
	return rows.Err()
}

func (r *PostgresRepository) readPotential(
	ctx context.Context, filter Filter, result *Snapshot,
) error {
	return r.pool.QueryRow(ctx, `SELECT COALESCE(sum(tm.base_minutes),0)::float8
		FROM sage.action_queue aq LEFT JOIN sage.databases d ON d.id=aq.database_id
		JOIN sage.toil_model tm ON tm.action_type=aq.action_type AND tm.effective_to IS NULL
		WHERE aq.status='pending' AND ($1='' OR d.name=$1)
		AND ($2::timestamptz IS NULL OR aq.proposed_at >= $2)
		AND ($3::timestamptz IS NULL OR aq.proposed_at <= $3)`, filter.Database,
		nullableTime(filter.Since), nullableTime(filter.Until)).Scan(&result.PotentialMinutes)
}

func (r *PostgresRepository) readIncidents(
	ctx context.Context, filter Filter, result *Snapshot,
) error {
	rows, err := r.pool.Query(ctx, `SELECT ia.kind, ia.severity, ia.evidence_id,
		ia.occurred_at, ia.credited_minutes::float8 FROM sage.incident_avoided ia
		LEFT JOIN sage.databases d ON d.id=ia.database_id WHERE ($1='' OR d.name=$1)
		AND ($2::timestamptz IS NULL OR ia.occurred_at >= $2)
		AND ($3::timestamptz IS NULL OR ia.occurred_at <= $3)
		ORDER BY ia.occurred_at`, filter.Database, nullableTime(filter.Since),
		nullableTime(filter.Until))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item Incident
		var minutes float64
		if err := rows.Scan(&item.Kind, &item.Severity, &item.EvidenceID,
			&item.OccurredAt, &minutes); err != nil {
			return err
		}
		result.IncidentMinutes += minutes
		result.Incidents = append(result.Incidents, item)
	}
	return rows.Err()
}

func nullableTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func sameMonth(left, right time.Time) bool {
	ly, lm, _ := left.Date()
	ry, rm, _ := right.Date()
	return ly == ry && lm == rm
}

func startOfWeek(value time.Time) time.Time {
	day := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -offset)
}

func databaseRows(values map[string]float64) []DatabaseMinutes {
	result := make([]DatabaseMinutes, 0, len(values))
	for name, minutes := range values {
		result = append(result, DatabaseMinutes{name, minutes})
	}
	return result
}

func dayRows(values map[string]float64) []DayMinutes {
	result := make([]DayMinutes, 0, len(values))
	for day, minutes := range values {
		result = append(result, DayMinutes{day, minutes})
	}
	return result
}
