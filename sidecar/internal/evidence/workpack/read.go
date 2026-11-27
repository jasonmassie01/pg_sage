package workpack

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// What a build reads from the history store, in one read-only transaction
// bounded by readTimeout per statement. The rollup tables and snapshots
// are only read (evid-01a/01b own them).

// readTimeout bounds each statement of a build.
const readTimeout = "30s"

type hourRow struct {
	qid    int64
	hour   time.Time
	calls  int64
	execMs float64
	rows   int64
}

type dayRow struct {
	qid    int64
	day    time.Time
	calls  int64
	execMs float64
	rows   int64
}

// history is the window's history. hourly and daily are in (queryid,
// time) order; covered is every coverage hour of the window; dbSize and
// tables are keyed by UTC day ("2006-01-02"), tables by "schema.name" of
// the largest tables (largest, by size then name).
type history struct {
	hourly   []hourRow
	daily    []dayRow
	covered  []time.Time
	coverage querystore.CoverageReport
	texts    map[int64]string
	known    map[string][]string
	dbSize   map[string]int64
	tables   map[string]map[string]int64
	largest  []string
	maint    maintHistory
	cat      catalogData
}

// Build reads db's history store (histstore.Resolve) for req's window and
// returns its pack. It writes nothing; Save stores the pack.
func Build(ctx context.Context, db any, req Request) (*Pack, error) {
	req.Window = req.Window.UTC()
	if err := req.Window.Validate(); err != nil {
		return nil, err
	}
	plan := planDays(req.Window, req.HourlyDays, req.Now)
	h, err := readHistory(ctx, histstore.Resolve(db), req, plan)
	if err != nil {
		return nil, err
	}
	if h.cat, err = readCatalog(ctx, db, req, catalogInputOf(req, plan, h)); err != nil {
		return nil, err
	}
	return assemble(req, plan, h)
}

// readHistory reads the window in one read-only transaction.
func readHistory(ctx context.Context, st histstore.Store, req Request,
	plan dayPlan) (history, error) {
	w := req.Window
	tx, err := beginRead(ctx, st)
	if err != nil {
		return history{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	r := reader{st: st.WithDB(tx), w: w}
	var h history
	steps := []func(context.Context, *history) error{
		func(ctx context.Context, h *history) error { return r.hourly(ctx, h, plan) },
		func(ctx context.Context, h *history) error { return r.daily(ctx, h, plan) },
		r.covered, r.coverage, r.snapshots, r.relations, r.settings, r.measures,
		r.deadTuples,
		func(ctx context.Context, h *history) error {
			return r.connections(ctx, h, req.location().String())
		},
	}
	for _, step := range steps {
		if err := step(ctx, &h); err != nil {
			return history{}, err
		}
	}
	return h, nil
}

// beginRead opens the build's transaction: read-only, each statement
// bounded by readTimeout (the store may be the monitored database).
func beginRead(ctx context.Context, st histstore.Store) (pgx.Tx, error) {
	tx, err := st.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("workpack: open a read of the history: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("workpack: make the history read read-only: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+readTimeout+"'"); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("workpack: bound the history read: %w", err)
	}
	return tx, nil
}

// reader runs the build's statements on one transaction of the store.
type reader struct {
	st histstore.Store
	w  Window
}

// fail names the table and the window a read failed on.
func (r reader) fail(table string, err error) error {
	return fmt.Errorf("workpack: read %s for [%s, %s): %w", table,
		r.w.From.Format(time.RFC3339), r.w.To.Format(time.RFC3339), err)
}

const hourlySQL = `/* pg_sage */ SELECT h.queryid, h.hour, h.calls, h.exec_ms, h.rows
  FROM sage.query_rollup_hourly h
 WHERE {db:h} AND h.hour >= $1 AND h.hour < $2
 ORDER BY h.queryid, h.hour`

func (r reader) hourly(ctx context.Context, h *history, plan dayPlan) error {
	if !plan.hourlyFrom.Before(r.w.To) {
		return nil
	}
	rows, err := r.st.Query(ctx, hourlySQL, plan.hourlyFrom, r.w.To)
	if err != nil {
		return r.fail("sage.query_rollup_hourly", err)
	}
	h.hourly, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (hourRow, error) {
		var x hourRow
		err := row.Scan(&x.qid, &x.hour, &x.calls, &x.execMs, &x.rows)
		x.hour = x.hour.UTC()
		return x, err
	})
	if err != nil {
		return r.fail("sage.query_rollup_hourly", err)
	}
	return nil
}

const dailySQL = `/* pg_sage */ SELECT d.queryid, d.day, d.calls, d.exec_ms, d.rows
  FROM sage.query_rollup_daily d
 WHERE {db:d} AND d.day = ANY($1::date[])
 ORDER BY d.queryid, d.day`

func (r reader) daily(ctx context.Context, h *history, plan dayPlan) error {
	if len(plan.dailyDays) == 0 {
		return nil
	}
	rows, err := r.st.Query(ctx, dailySQL, plan.dailyDays)
	if err != nil {
		return r.fail("sage.query_rollup_daily", err)
	}
	h.daily, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (dayRow, error) {
		var x dayRow
		err := row.Scan(&x.qid, &x.day, &x.calls, &x.execMs, &x.rows)
		return x, err
	})
	if err != nil {
		return r.fail("sage.query_rollup_daily", err)
	}
	return nil
}

const coveredSQL = `/* pg_sage */ SELECT c.hour FROM sage.query_rollup_coverage c
 WHERE {db:c} AND c.hour >= $1 AND c.hour < $2 ORDER BY c.hour`

func (r reader) covered(ctx context.Context, h *history) error {
	rows, err := r.st.Query(ctx, coveredSQL, r.w.From, r.w.To)
	if err != nil {
		return r.fail("sage.query_rollup_coverage", err)
	}
	h.covered, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Time, error) {
		var t time.Time
		err := row.Scan(&t)
		return t.UTC(), err
	})
	if err != nil {
		return r.fail("sage.query_rollup_coverage", err)
	}
	return nil
}

func (r reader) coverage(ctx context.Context, h *history) error {
	rep, err := querystore.RollupCoverage(ctx, r.st, r.w.From, r.w.To)
	if err != nil {
		if errors.Is(err, histstore.ErrNotRegistered) {
			return err
		}
		return r.fail("the rollup coverage", err)
	}
	h.coverage = rep
	return nil
}
