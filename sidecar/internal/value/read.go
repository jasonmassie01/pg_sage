package value

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
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

// realizedSQL sums credited minutes per action type and day in SQL, over
// idx_action_log_value_credit (credited actions only; perf v1.8.3,
// perf-selfexcl): rows read are the credited actions in the window, never
// the rest of the ledger. The window bounds are an index range in the
// generic plan too ($1 IS NULL OR ... was not). $3 and $4 start this
// month and this week.
const realizedSQL = `SELECT al.action_type,
	date_trunc('day', al.executed_at)::date::text,
	sum(al.toil_minutes_saved)::float8,
	COALESCE(sum(al.toil_minutes_saved) FILTER (WHERE al.executed_at >= $3), 0)::float8,
	COALESCE(sum(al.toil_minutes_saved) FILTER (WHERE al.executed_at >= $4), 0)::float8
	FROM sage.action_log al
	WHERE al.outcome = 'success' AND al.toil_minutes_saved IS NOT NULL
	  AND al.executed_at >= COALESCE($1::timestamptz, '-infinity')
	  AND al.executed_at <= COALESCE($2::timestamptz, 'infinity')
	GROUP BY 1, 2`

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readRealized(
	ctx context.Context, q querier, name string, filter Filter, result *Snapshot,
) error {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := q.Query(ctx, realizedSQL, nullableTime(filter.Since),
		nullableTime(filter.Until), monthStart, startOfWeek(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	byDB := map[string]float64{}
	byDay := map[string]float64{}
	for rows.Next() {
		var feature, day string
		var minutes, month, week float64
		if err := rows.Scan(&feature, &day, &minutes, &month, &week); err != nil {
			return err
		}
		result.AllTimeMinutes += minutes
		result.MonthMinutes += month
		result.WeekMinutes += week
		result.ByFeatureMinutes[feature] += minutes
		byDB[name] += minutes
		byDay[day] += minutes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	result.ByDatabaseMinutes = databaseRows(byDB)
	result.TrendMinutes = dayRows(byDay)
	return nil
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
