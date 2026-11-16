package querystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// The daily tier of the per-queryid rollup (evid-01b). Hourly rows are
// kept 35 days and daily rows 400; a UTC day's daily row per queryid is
// the sum of its hourly rows, rolled once the day's last hour is. Coverage
// stays hourly (sage.query_rollup_coverage is kept as long as the daily
// tier) and is summed on read.

// ErrRollupDay marks a day the daily roll refuses: zero, or not over yet.
var ErrRollupDay = errors.New("query rollup: invalid day")

const dayLen = 24 * time.Hour

// dailyDaysPerPass bounds the days one worker pass rolls.
const dailyDaysPerPass = 7

// dailyDueAfter is how far into the next UTC day a day is rolled (00:15).
const dailyDueAfter = 15 * time.Minute

// dailySettledAfter is how long after its end the newest day is re-rolled:
// its 23:00 hour is re-rolled as h-2 at 01:05.
const dailySettledAfter = 75 * time.Minute

// DayReport is what one day's roll found and wrote.
type DayReport struct {
	Day      time.Time
	Queryids int   // queryids with an hourly row in the day
	Written  int64 // daily rows inserted or replaced
}

// checkDay truncates d to its UTC day and refuses a zero day and one that
// has not ended.
func checkDay(d time.Time) (time.Time, error) {
	if d.IsZero() {
		return time.Time{}, fmt.Errorf("%w: zero day", ErrRollupDay)
	}
	t := d.UTC().Truncate(dayLen)
	if t.Add(dayLen).After(time.Now()) {
		return time.Time{}, fmt.Errorf("%w: day %s is today or in the future",
			ErrRollupDay, t.Format(time.DateOnly))
	}
	return t, nil
}

// rollupDayErr wraps a failed step of day d's roll, naming the statement
// bound when it was hit.
func rollupDayErr(d time.Time, step string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return fmt.Errorf("query rollup day %s: %s: statement_timeout %s exceeded: %w",
			d.Format(time.DateOnly), step, rollupStatementTimeout, err)
	}
	return fmt.Errorf("query rollup day %s: %s: %w", d.Format(time.DateOnly), step, err)
}

// RollupDay sums day d (a closed UTC day) of db's hourly rollup into its
// daily rows, in one transaction under the rollup's advisory lock and
// statement bound. A re-roll replaces a row only when it finds at least
// as many hours as the row holds: once retention removed part of a day's
// hours, its daily row is never shrunk. It writes only sage history.
func RollupDay(ctx context.Context, db any, d time.Time) (DayReport, error) {
	t, err := checkDay(d)
	if err != nil {
		return DayReport{}, err
	}
	st := histstore.Resolve(db)
	tx, err := st.Begin(ctx)
	if err != nil {
		return DayReport{}, rollupDayErr(t, "begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txs := st.WithDB(tx)
	bound := fmt.Sprintf("SET LOCAL statement_timeout = %d",
		rollupStatementTimeout.Milliseconds())
	if _, err := tx.Exec(ctx, bound); err != nil {
		return DayReport{}, rollupDayErr(t, "set the statement bound", err)
	}
	if _, err := tx.Exec(ctx, "/* pg_sage */ SELECT pg_advisory_xact_lock(hashtext($1), $2)",
		rollupLockKey, int32(st.DatabaseID())); err != nil {
		return DayReport{}, rollupDayErr(t, "take the rollup lock", err)
	}
	rep := DayReport{Day: t}
	if err := txs.QueryRow(ctx, rollupDaySQL, t, t, t.Add(dayLen)).Scan(&rep.Queryids,
		&rep.Written); err != nil {
		return DayReport{}, rollupDayErr(t, "sum sage.query_rollup_hourly into "+
			"sage.query_rollup_daily", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DayReport{}, rollupDayErr(t, "commit", err)
	}
	return rep, nil
}

// dayDue reports whether day d is due at now: its last hour is rolled
// (through, the newest rolled hour) and now is past 00:15 of the next day.
func dayDue(d, now, through time.Time) bool {
	return !through.IsZero() && !through.Before(d.Add(23*time.Hour)) &&
		!now.Before(d.Add(dayLen+dailyDueAfter))
}

// reRollNewest reports whether the newest daily day d is re-rolled at now:
// until its last hour's late re-roll has landed.
func reRollNewest(d, now time.Time) bool {
	return now.Before(d.Add(dayLen + dailySettledAfter))
}

// rollDays rolls the days due now, oldest first and at most
// dailyDaysPerPass, after re-rolling the newest daily day while it may
// still change. Days without an hourly row are skipped.
func (w *RollupWorker) rollDays(ctx context.Context) (int, error) {
	now := w.now()
	st := histstore.Resolve(w.Store)
	var through, newest *time.Time
	if err := st.QueryRow(ctx, dayResumeSQL).Scan(&through, &newest); err != nil {
		return 0, fmt.Errorf("query rollup of %s: find the days to roll in "+
			"sage.query_rollup_coverage and sage.query_rollup_daily: %w", w.Database, err)
	}
	if through == nil {
		return 0, nil
	}
	var cursor time.Time
	if newest != nil {
		cursor = newest.UTC().Add(dayLen)
		if reRollNewest(newest.UTC(), now) {
			if _, err := RollupDay(ctx, w.Store, newest.UTC()); err != nil {
				return 0, err
			}
		}
	}
	rolled := 0
	for rolled < dailyDaysPerPass {
		var next *time.Time
		if err := st.QueryRow(ctx, nextHourlySQL, cursor).Scan(&next); err != nil {
			return rolled, fmt.Errorf("query rollup of %s: find the next day in "+
				"sage.query_rollup_hourly: %w", w.Database, err)
		}
		if next == nil {
			break
		}
		d := next.UTC().Truncate(dayLen)
		if !dayDue(d, now, through.UTC()) {
			break
		}
		if _, err := RollupDay(ctx, w.Store, d); err != nil {
			return rolled, err
		}
		rolled++
		cursor = d.Add(dayLen)
	}
	return rolled, nil
}

// RebuildHourly rolls every closed hour of db's history store again from
// its oldest queries snapshot, but no further back than days (0: no
// limit): after a history migration the hourly tier, which is derived and
// not copied, is rebuilt from the copied snapshots. It returns the hours
// rolled.
func RebuildHourly(ctx context.Context, db any, days int) (int, error) {
	st := histstore.Resolve(db)
	var oldest *time.Time
	if err := st.QueryRow(ctx, oldestQueriesSnapshotSQL).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("rebuild the hourly query rollup: find the oldest queries "+
			"snapshot in sage.snapshots: %w", err)
	}
	if oldest == nil {
		return 0, nil
	}
	now := time.Now().UTC().Truncate(time.Hour)
	from := oldest.UTC().Truncate(time.Hour)
	if floor := now.Add(-time.Duration(days) * dayLen); days > 0 && from.Before(floor) {
		from = floor
	}
	n := 0
	for h := from; h.Before(now); h = h.Add(time.Hour) {
		if _, err := RollupHour(ctx, st, h); err != nil {
			return n, fmt.Errorf("rebuild the hourly query rollup: %w", err)
		}
		n++
	}
	return n, nil
}
