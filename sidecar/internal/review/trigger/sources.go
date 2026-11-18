package trigger

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// Source is a producer the scheduler polls: it reads what pg_sage already
// stores (pg_sage's own sage tables, never the workload's) and returns the
// triggers it would fire now. Firing is idempotent per dedupe key, so a
// source may return the same trigger on every poll.
type Source interface {
	Kind() Kind
	Evaluate(ctx context.Context, now time.Time) ([]Trigger, error)
}

type sourceFunc struct {
	kind Kind
	fn   func(ctx context.Context, now time.Time) ([]Trigger, error)
}

func (s sourceFunc) Kind() Kind { return s.kind }

func (s sourceFunc) Evaluate(ctx context.Context, now time.Time) ([]Trigger, error) {
	return s.fn(ctx, now)
}

// readTimeout bounds each statement of a source's read.
const readTimeout = "10s"

// readOnly runs fn in a read-only transaction whose statements are bounded
// by readTimeout: the history may live in the monitored database.
func readOnly(ctx context.Context, st histstore.Store, fn func(histstore.Store) error) error {
	tx, err := st.Begin(ctx)
	if err != nil {
		return fmt.Errorf("open a read of the history: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := bound(ctx, tx); err != nil {
		return err
	}
	return fn(st.WithDB(tx))
}

// bound makes tx read-only with each statement bounded by readTimeout.
func bound(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return fmt.Errorf("make the read read-only: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+readTimeout+"'"); err != nil {
		return fmt.Errorf("bound the read: %w", err)
	}
	return nil
}

const coverageSQL = `/* pg_sage review trigger */
SELECT count(DISTINCT c.hour) FILTER (WHERE c.hour >= $1), min(c.hour)
  FROM sage.query_rollup_coverage c WHERE {db:c} AND c.hour < $2`

// EnrollmentSource fires the first full review once the rollups hold two
// complete weekly cycles at 80% coverage, unless a full review exists.
func EnrollmentSource(store *Store, hist histstore.Store, database string) Source {
	return sourceFunc{kind: KindEnrollment, fn: func(ctx context.Context,
		now time.Time) ([]Trigger, error) {
		if _, done, err := store.LastFullReview(ctx, database); err != nil || done {
			return nil, err
		}
		end := now.UTC().Truncate(time.Hour)
		var hours int
		var oldest *time.Time
		err := readOnly(ctx, hist, func(st histstore.Store) error {
			return st.QueryRow(ctx, coverageSQL, end.Add(-EnrollmentWindow), end).
				Scan(&hours, &oldest)
		})
		if err != nil {
			return nil, fmt.Errorf("read sage.query_rollup_coverage of %s: %w", database, err)
		}
		if oldest == nil || !EnrollmentReady(hours, *oldest, end) {
			return nil, nil
		}
		window := int(EnrollmentWindow / time.Hour)
		return []Trigger{{Database: database, Kind: KindEnrollment, Key: "first",
			Detail: map[string]any{"hours_covered": hours, "window_hours": window,
				"coverage_pct": float64(hours*100) / float64(window),
				"oldest_hour":  oldest.UTC().Format(time.RFC3339)}}}, nil
	}}
}

// CalendarSource fires calendar days after the last full review.
func CalendarSource(store *Store, database string, days int) Source {
	return sourceFunc{kind: KindCalendar, fn: func(ctx context.Context,
		now time.Time) ([]Trigger, error) {
		last, ok, err := store.LastFullReview(ctx, database)
		if err != nil || !ok || !CalendarDue(last.FinishedAt, now, days) {
			return nil, err
		}
		return []Trigger{{Database: database, Kind: KindCalendar,
			Key: fmt.Sprintf("after:%d", last.ID), Detail: map[string]any{
				"last_review_id": last.ID, "calendar_days": days,
				"last_finished_at": last.FinishedAt.Format(time.RFC3339)}}}, nil
	}}
}

// RegressionOpenFor is how long a regression finding stays open before it
// triggers a review.
const RegressionOpenFor = 72 * time.Hour

const regressionSQL = `/* pg_sage review trigger */
SELECT f.id, f.category, f.title, f.created_at,
       EXISTS (SELECT 1 FROM sage.action_log a
                WHERE a.finding_id = f.id AND a.outcome = 'rolled_back')
  FROM sage.findings f
 WHERE f.status = 'open' AND f.category IN ('query_regression', 'plan_regression')
   AND f.created_at <= $1
   AND NOT EXISTS (SELECT 1 FROM sage.action_log a
                     JOIN sage.action_outcome o ON o.action_log_id = a.id
                    WHERE a.finding_id = f.id AND a.outcome = 'success'
                      AND o.verdict = 'improved')
 ORDER BY f.id LIMIT 50`

// RegressionSource fires for a regression finding open for 72 hours or
// more whose action was reverted, or that has no verified action: none
// that ran and was judged improved by verification (sage.action_outcome).
// An action that ran but is still pending, neutral or unverifiable has
// not resolved the regression.
func RegressionSource(store *Store, database string) Source {
	return sourceFunc{kind: KindUnresolvedRegression, fn: func(ctx context.Context,
		now time.Time) ([]Trigger, error) {
		if err := store.usable(); err != nil {
			return nil, err
		}
		out, err := readRegressions(ctx, store, database, now.Add(-RegressionOpenFor))
		if err != nil {
			return nil, fmt.Errorf("read the open regressions of %s: %w", database, err)
		}
		return out, nil
	}}
}

func readRegressions(ctx context.Context, store *Store, database string,
	openedBy time.Time) ([]Trigger, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := bound(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, regressionSQL, openedBy)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Trigger, error) {
		return scanRegression(row, database)
	})
}

func scanRegression(row pgx.CollectableRow, database string) (Trigger, error) {
	var id int64
	var category, title string
	var opened time.Time
	var reverted bool
	if err := row.Scan(&id, &category, &title, &opened, &reverted); err != nil {
		return Trigger{}, err
	}
	reason := "no verified action"
	if reverted {
		reason = "action reverted"
	}
	return Trigger{Database: database, Kind: KindUnresolvedRegression,
		Key: fmt.Sprintf("finding:%d", id), Detail: map[string]any{"finding_id": id,
			"category": category, "title": title, "reason": reason,
			"open_since": opened.UTC().Format(time.RFC3339)}}, nil
}

var sizesSQL = `/* pg_sage review trigger */
SELECT (s.collected_at AT TIME ZONE 'UTC')::date,
       max((` + snapstore.DataSQL("s") + `->>'db_size_bytes')::int8)
  FROM sage.snapshots s
 WHERE {db:s} AND s.category = 'system' AND s.collected_at >= $1 AND s.collected_at < $2
 GROUP BY 1 HAVING max((` + snapstore.DataSQL("s") + `->>'db_size_bytes')::int8) IS NOT NULL
 ORDER BY 1`

// growthDays is how many days of sizes the growth source reads.
const growthDays = 29

// CostGrowthSource fires when the newest whole day's size growth is a
// robust outlier against the days before (GrowthAnomaly).
func CostGrowthSource(hist histstore.Store, database string) Source {
	return sourceFunc{kind: KindCostGrowth, fn: func(ctx context.Context,
		now time.Time) ([]Trigger, error) {
		end := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		var sizes []DaySize
		err := readOnly(ctx, hist, func(st histstore.Store) error {
			rows, err := st.Query(ctx, sizesSQL, end.AddDate(0, 0, -growthDays), end)
			if err != nil {
				return err
			}
			sizes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (DaySize, error) {
				var d DaySize
				err := row.Scan(&d.Day, &d.Bytes)
				d.Day = d.Day.UTC()
				return d, err
			})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("read the daily sizes of %s: %w", database, err)
		}
		a, ok := GrowthAnomaly(sizes)
		if !ok {
			return nil, nil
		}
		day := a.Day.Format(time.DateOnly)
		return []Trigger{{Database: database, Kind: KindCostGrowth, Key: "day:" + day,
			Detail: map[string]any{"day": day, "growth_bytes": a.GrowthBytes,
				"median_growth_bytes": a.MedianBytes, "z": a.Z, "threshold_z": a.ThresholdZ,
				"history_days": a.HistoryDays, "previous_size_bytes": a.PreviousSize}}}, nil
	}}
}

// FactsFunc reads an instance's facts now; ok is false when no adapter
// knows them (the source then fires nothing).
type FactsFunc func(ctx context.Context) (Facts, bool, error)

// InstanceSource fires when a known instance fact (class, vCPUs, memory)
// differs from the last one it saved. The first observation is saved, not
// fired.
func InstanceSource(store *Store, database string, facts FactsFunc) Source {
	return sourceFunc{kind: KindInstanceChange, fn: func(ctx context.Context,
		now time.Time) ([]Trigger, error) {
		cur, ok, err := facts(ctx)
		if err != nil || !ok {
			return nil, err
		}
		prev, seen, err := store.LoadFacts(ctx, database)
		if err != nil {
			return nil, err
		}
		if seen && !InstanceChanged(prev, cur) {
			return nil, nil
		}
		var out []Trigger
		if seen {
			out = []Trigger{instanceTrigger(database, prev, cur, now)}
			if _, _, err := store.Fire(ctx, out[0]); err != nil {
				return nil, err // keep the old facts so the next poll fires again
			}
		}
		return out, store.SaveFacts(ctx, database, cur)
	}}
}

func instanceTrigger(database string, prev, cur Facts, now time.Time) Trigger {
	return Trigger{Database: database, Kind: KindInstanceChange,
		Key: fmt.Sprintf("change:%d", now.Unix()), Detail: map[string]any{
			"from_class": prev.Class, "to_class": cur.Class, "from_vcpus": prev.VCPUs,
			"to_vcpus": cur.VCPUs, "from_memory_bytes": prev.MemoryBytes,
			"to_memory_bytes": cur.MemoryBytes, "source": cur.Source}}
}
