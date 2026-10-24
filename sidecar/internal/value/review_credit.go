package value

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChangeReviewFeature is the by_feature key of Change Review credits (the
// Value page renders it "Change Review").
const ChangeReviewFeature = "change_review"

// reviewCreditSQL sums final Change Review credits per day of
// credit_final_at, over idx_change_review_credit (credit_final_at INCLUDE
// credited_minutes WHERE credit_state = 'final'): an index-only range, never
// the rest of the review table. $3 and $4 start this month and this week.
const reviewCreditSQL = `/* pg_sage change_review_value_credit v1 */
SELECT date_trunc('day', credit_final_at)::date::text, sum(credited_minutes)::float8,
	COALESCE(sum(credited_minutes) FILTER (WHERE credit_final_at >= $3), 0)::float8,
	COALESCE(sum(credited_minutes) FILTER (WHERE credit_final_at >= $4), 0)::float8
FROM sage.change_review
WHERE credit_state = 'final'
  AND credit_final_at >= COALESCE($1::timestamptz, '-infinity')
  AND credit_final_at <= COALESCE($2::timestamptz, 'infinity')
GROUP BY 1`

// reviewPendingSQL sums the minutes of concurred reviews whose receipt is
// pending, by DBA decision time (unbounded without a window, like
// readPotential). A pending credit always waits on a pending receipt of
// an awaiting or landed change, so each half reads one partial index
// (change_review_landing_awaiting, change_review_receipt_due).
const reviewPendingSQL = `/* pg_sage change_review_value_pending v1 */
SELECT (SELECT COALESCE(sum(credited_minutes), 0) FROM sage.change_review
         WHERE landing_state = 'awaiting' AND credit_state = 'pending'
           AND ($1::timestamptz IS NULL OR dba_decided_at >= $1)
           AND ($2::timestamptz IS NULL OR dba_decided_at <= $2))
     + (SELECT COALESCE(sum(credited_minutes), 0) FROM sage.change_review
         WHERE landing_state = 'landed' AND receipt_verdict = 'pending'
           AND credit_state = 'pending'
           AND ($1::timestamptz IS NULL OR dba_decided_at >= $1)
           AND ($2::timestamptz IS NULL OR dba_decided_at <= $2))`

// rowQuerier is a pool or a transaction.
type rowQuerier interface {
	querier
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readReviewCredits adds the database's final Change Review credits to the
// snapshot (totals, by_feature, by_database, trend) and its pending review
// minutes to the potential.
func readReviewCredits(ctx context.Context, q rowQuerier, name string, filter Filter,
	result *Snapshot) error {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := q.Query(ctx, reviewCreditSQL, nullableTime(filter.Since),
		nullableTime(filter.Until), monthStart, startOfWeek(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	byDay := minutesByDay(result.TrendMinutes)
	total := 0.0
	for rows.Next() {
		var day string
		var minutes, month, week float64
		if err := rows.Scan(&day, &minutes, &month, &week); err != nil {
			return err
		}
		total += minutes
		result.MonthMinutes += month
		result.WeekMinutes += week
		byDay[day] += minutes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total > 0 {
		result.AllTimeMinutes += total
		result.ByFeatureMinutes[ChangeReviewFeature] += total
		byDB := minutesByDatabase(result.ByDatabaseMinutes)
		byDB[name] += total
		result.ByDatabaseMinutes = databaseRows(byDB)
		result.TrendMinutes = dayRows(byDay)
	}
	var pending float64
	if err := q.QueryRow(ctx, reviewPendingSQL, nullableTime(filter.Since),
		nullableTime(filter.Until)).Scan(&pending); err != nil {
		return err
	}
	result.PotentialMinutes += pending
	return nil
}

func minutesByDay(rows []DayMinutes) map[string]float64 {
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Day] += r.Minutes
	}
	return out
}

func minutesByDatabase(rows []DatabaseMinutes) map[string]float64 {
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Name] += r.Minutes
	}
	return out
}
