package capacity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// ForecastDays is the horizon whose error the next reviews measure.
const ForecastDays = 30

// ForecastLog keeps each review's 30-day database size projection in
// sage.review_capacity_forecast and, once its target day has a measured
// size, how far off it was: the review's number.
type ForecastLog struct{ pool *pgxpool.Pool }

// NewForecastLog is a log over the sage schema's pool.
func NewForecastLog(pool *pgxpool.Pool) *ForecastLog { return &ForecastLog{pool: pool} }

// DaySize is a measured database size on a UTC day.
type DaySize struct {
	Day   time.Time
	Bytes float64
}

// ForecastError is a settled forecast. ErrorPct is (projected - measured)
// / measured in percent; nil when the measured size is 0.
type ForecastError struct {
	MadeOn, TargetOn time.Time
	ProjectedBytes   float64
	MeasuredBytes    float64
	ErrorPct         *float64
}

const recordForecastSQL = `/* pg_sage review:capacity */
INSERT INTO sage.review_capacity_forecast (database_name, made_on, target_on,
    projected_bytes, band_bytes)
VALUES ($1, $2::text::date, $3::text::date, $4, $5)
ON CONFLICT (database_name, made_on) DO NOTHING`

// settleForecastSQL measures, once, every pending forecast of $1 whose
// target day is one of the measured days ($2, $3): the row is updated
// only while measured_bytes is NULL, so a second pass is a no-op.
const settleForecastSQL = `/* pg_sage review:capacity */
UPDATE sage.review_capacity_forecast f
   SET measured_bytes = m.bytes, measured_at = now(),
       error_pct = CASE WHEN m.bytes > 0
                        THEN 100 * (f.projected_bytes - m.bytes) / m.bytes END
  FROM unnest($2::text[]::date[], $3::float8[]) AS m(day, bytes)
 WHERE f.database_name = $1 AND f.measured_bytes IS NULL AND f.target_on = m.day
RETURNING f.made_on, f.target_on, f.projected_bytes, f.measured_bytes, f.error_pct`

// Record stores p's 30-day projection, made on p.Origin, and reports
// whether it was new: a review day already recorded is kept as it is.
func (l *ForecastLog) Record(ctx context.Context, database string, p Projection,
) (bool, error) {
	if l == nil || l.pool == nil {
		return false, errors.New("capacity forecast log: no database")
	}
	a, ok := p.At(ForecastDays)
	if !ok {
		return false, fmt.Errorf("record the capacity forecast of %s: no %d-day projection",
			database, ForecastDays)
	}
	// Days go as text: a timestamptz cast to date would follow the session's
	// TimeZone.
	tag, err := l.pool.Exec(ctx, recordForecastSQL, database, dateText(p.Origin),
		dateText(p.Origin.AddDate(0, 0, ForecastDays)), a.Bytes, a.Band)
	if err != nil {
		return false, fmt.Errorf("record the capacity forecast of %s made on %s in "+
			"sage.review_capacity_forecast: %w", database, p.Origin.Format(time.DateOnly), err)
	}
	return tag.RowsAffected() == 1, nil
}

// Settle measures the pending forecasts whose target day is in sizes and
// returns the ones settled now.
func (l *ForecastLog) Settle(ctx context.Context, database string, sizes []DaySize,
) ([]ForecastError, error) {
	if l == nil || l.pool == nil {
		return nil, errors.New("capacity forecast log: no database")
	}
	days := make([]string, len(sizes))
	bytes := make([]float64, len(sizes))
	for i, s := range sizes {
		days[i], bytes[i] = dateText(s.Day), s.Bytes
	}
	rows, err := l.pool.Query(ctx, settleForecastSQL, database, days, bytes)
	if err != nil {
		return nil, fmt.Errorf("settle the capacity forecasts of %s in "+
			"sage.review_capacity_forecast: %w", database, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ForecastError, error) {
		var e ForecastError
		err := r.Scan(&e.MadeOn, &e.TargetOn, &e.ProjectedBytes, &e.MeasuredBytes,
			&e.ErrorPct)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("settle the capacity forecasts of %s: %w", database, err)
	}
	return out, nil
}

// DaySizes is s's measured days, the first being from's UTC day.
func DaySizes(s workpack.Series, from time.Time) []DaySize {
	var out []DaySize
	for i, v := range s.Values {
		if v != nil {
			out = append(out, DaySize{Day: dayStart(from).AddDate(0, 0, i), Bytes: *v})
		}
	}
	return out
}

// logForecast settles the forecasts the pack's sizes reach, notes their
// error, then records this review's projection.
func (g *Generator) logForecast(ctx context.Context, pack *workpack.Pack, env candidate.Env,
	cov *candidate.Coverage) error {
	if g.cfg.Log == nil {
		return nil
	}
	gr, err := readGrowth(pack)
	if err != nil {
		return fmt.Errorf("capacity generator for %s: %w", env.Database, err)
	}
	settled, err := g.cfg.Log.Settle(ctx, env.Database, DaySizes(gr.DBSize, pack.Window.From))
	if err != nil {
		return fmt.Errorf("capacity generator for %s: %w", env.Database, err)
	}
	for _, e := range settled {
		if e.ErrorPct != nil {
			cov.Note("forecast error: the %d-day projection made on %s was off by %+.1f%%",
				ForecastDays, e.MadeOn.Format(time.DateOnly), *e.ErrorPct)
		}
	}
	// Without a projection there is nothing to record; Build noted why.
	if p, perr := Project(gr.DBSize, pack.Window.From); perr == nil {
		if _, err := g.cfg.Log.Record(ctx, env.Database, p); err != nil {
			return fmt.Errorf("capacity generator for %s: %w", env.Database, err)
		}
	}
	return nil
}

// dateText is t's UTC day as PostgreSQL reads a date.
func dateText(t time.Time) string { return t.UTC().Format(time.DateOnly) }
