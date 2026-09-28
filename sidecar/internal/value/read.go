package value

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// readSnapshot reads one monitored database's ledger. Every row in the
// pool belongs to that database, so rows are labelled with the fleet
// instance name instead of joining sage.databases (D3).
func readSnapshot(
	ctx context.Context, pool *pgxpool.Pool, name string, filter Filter,
) (Snapshot, error) {
	result := Snapshot{ByFeatureMinutes: map[string]float64{}}
	if err := readRealized(ctx, pool, name, filter, &result); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot realized value: %w", err)
	}
	if err := readPotential(ctx, pool, filter, &result); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot potential value: %w", err)
	}
	if err := readIncidents(ctx, pool, filter, &result); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot incidents: %w", err)
	}
	return result, nil
}

func readRealized(
	ctx context.Context, pool *pgxpool.Pool, name string, filter Filter,
	result *Snapshot,
) error {
	rows, err := pool.Query(ctx, `SELECT al.action_type,
		date_trunc('day', al.executed_at)::date::text, al.executed_at,
		al.toil_minutes_saved::float8 FROM sage.action_log al
		WHERE al.outcome='success' AND al.toil_minutes_saved IS NOT NULL
		AND ($1::timestamptz IS NULL OR al.executed_at >= $1)
		AND ($2::timestamptz IS NULL OR al.executed_at <= $2)
		ORDER BY al.executed_at`, nullableTime(filter.Since),
		nullableTime(filter.Until))
	if err != nil {
		return err
	}
	defer rows.Close()
	byDB := map[string]float64{}
	byDay := map[string]float64{}
	now := time.Now().UTC()
	for rows.Next() {
		var feature, day string
		var at time.Time
		var minutes float64
		if err := rows.Scan(&feature, &day, &at, &minutes); err != nil {
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
		byDB[name] += minutes
		byDay[day] += minutes
	}
	result.ByDatabaseMinutes = databaseRows(byDB)
	result.TrendMinutes = dayRows(byDay)
	return rows.Err()
}

func readPotential(
	ctx context.Context, pool *pgxpool.Pool, filter Filter, result *Snapshot,
) error {
	return pool.QueryRow(ctx, `SELECT COALESCE(sum(tm.base_minutes),0)::float8
		FROM sage.action_queue aq
		JOIN sage.toil_model tm ON tm.action_type=aq.action_type AND tm.effective_to IS NULL
		WHERE aq.status='pending'
		AND ($1::timestamptz IS NULL OR aq.proposed_at >= $1)
		AND ($2::timestamptz IS NULL OR aq.proposed_at <= $2)`,
		nullableTime(filter.Since), nullableTime(filter.Until)).Scan(&result.PotentialMinutes)
}

func readIncidents(
	ctx context.Context, pool *pgxpool.Pool, filter Filter, result *Snapshot,
) error {
	rows, err := pool.Query(ctx, `SELECT ia.kind, ia.severity, ia.evidence_id,
		ia.occurred_at, ia.credited_minutes::float8 FROM sage.incident_avoided ia
		WHERE ($1::timestamptz IS NULL OR ia.occurred_at >= $1)
		AND ($2::timestamptz IS NULL OR ia.occurred_at <= $2)
		ORDER BY ia.occurred_at`, nullableTime(filter.Since),
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

// databaseRows orders databases by minutes descending, then name, so the
// report is stable across requests (G2-B25).
func databaseRows(values map[string]float64) []DatabaseMinutes {
	result := make([]DatabaseMinutes, 0, len(values))
	for name, minutes := range values {
		result = append(result, DatabaseMinutes{name, minutes})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Minutes != result[j].Minutes {
			return result[i].Minutes > result[j].Minutes
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// dayRows orders the daily trend by day ascending (G2-B25).
func dayRows(values map[string]float64) []DayMinutes {
	result := make([]DayMinutes, 0, len(values))
	for day, minutes := range values {
		result = append(result, DayMinutes{day, minutes})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Day < result[j].Day
	})
	return result
}
