package querystore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// evid-01a without a database: the hour checks, the error paths, the sum
// of snapshot intervals and the worker's due hour.

// recTx is a transaction whose reads fail with err and that records the
// arguments of each statement.
type recTx struct {
	pgx.Tx
	err  error
	sqls []string
	args [][]any
}

func (r *recTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sqls, r.args = append(r.sqls, sql), append(r.args, args)
	return pgconn.CommandTag{}, nil
}

func (r *recTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sqls, r.args = append(r.sqls, sql), append(r.args, args)
	return nil, r.err
}

func (r *recTx) Commit(context.Context) error   { return errors.New("recTx: unexpected commit") }
func (r *recTx) Rollback(context.Context) error { return nil }

type recDB struct{ tx *recTx }

func (d recDB) Begin(context.Context) (pgx.Tx, error) { return d.tx, nil }

func TestRollupHour_RejectsZeroAndFutureHours(t *testing.T) {
	db := recDB{tx: &recTx{}}
	for name, hour := range map[string]time.Time{
		"zero":   {},
		"future": time.Now().Add(2 * time.Hour),
	} {
		_, err := RollupHour(context.Background(), db, hour)
		if !errors.Is(err, ErrRollupHour) {
			t.Fatalf("%s hour: err = %v, want ErrRollupHour", name, err)
		}
		if name == "future" && !strings.Contains(err.Error(),
			hour.UTC().Truncate(time.Hour).Format(time.RFC3339)) {
			t.Fatalf("future hour error does not name the hour: %v", err)
		}
	}
	if len(db.tx.sqls) != 0 {
		t.Fatalf("a rejected hour ran %d statements", len(db.tx.sqls))
	}
}

func TestRollupHour_NilStoreIsNoDatabase(t *testing.T) {
	_, err := RollupHour(context.Background(), nil, time.Now().Add(-2*time.Hour))
	if !errors.Is(err, histstore.ErrNoDatabase) {
		t.Fatalf("err = %v, want histstore.ErrNoDatabase", err)
	}
}

func TestRollupHour_ReadFailureNamesTheSnapshotsAndTheHour(t *testing.T) {
	tx := &recTx{err: errors.New("connection reset by peer")}
	hour := time.Date(2026, 11, 2, 10, 37, 12, 5, time.UTC)
	_, err := RollupHour(context.Background(), recDB{tx: tx}, hour)
	if err == nil || !strings.Contains(err.Error(), "sage.snapshots") ||
		!strings.Contains(err.Error(), "2026-11-02T10:00:00Z") ||
		!strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("err = %v, want sage.snapshots, the truncated hour and the cause", err)
	}
	if len(tx.sqls) < 3 || !strings.Contains(tx.sqls[0], "statement_timeout") ||
		!strings.Contains(tx.sqls[1], "pg_advisory_xact_lock") {
		t.Fatalf("statements = %q; want the bound, then the lock, then the read", tx.sqls)
	}
	from, ok := tx.args[2][0].(time.Time)
	if !ok || !from.Equal(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("read from %v, want the hour truncated to 10:00 UTC", tx.args[2][0])
	}
}

func TestRollupHour_TimeoutNamesTheBound(t *testing.T) {
	tx := &recTx{err: &pgconn.PgError{Code: "57014", Message: "canceling statement"}}
	_, err := RollupHour(context.Background(), recDB{tx: tx}, time.Now().Add(-3*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "statement_timeout 30s") {
		t.Fatalf("err = %v, want the 30s bound named", err)
	}
}

func TestSumIntervals_BreaksCountNothingAndFlagTheRow(t *testing.T) {
	ivs := []SnapshotInterval{
		{QueryID: 2, Calls: 3, ExecMs: 1.5, Rows: 3, WALBytes: 10},
		{QueryID: 1, Calls: 5, ExecMs: 2, SharedBlksHit: 4},
		{QueryID: 2, EpochBreak: true},
		{QueryID: 2, Calls: 4, ExecMs: 0.5, TempBlksWritten: 2},
		{QueryID: 3, EpochBreak: true},
	}
	got := sumIntervals(ivs)
	if len(got) != 3 || got[0].queryID != 1 || got[1].queryID != 2 || got[2].queryID != 3 {
		t.Fatalf("rows = %+v, want queryids 1, 2, 3 in order", got)
	}
	q2 := got[1]
	if q2.calls != 7 || q2.execMs != 2 || q2.rows != 3 || q2.wal != 10 || q2.twrite != 2 ||
		!q2.epochBreak || q2.pairs != 2 {
		t.Fatalf("queryid 2 = %+v", q2)
	}
	if q3 := got[2]; !q3.epochBreak || q3.pairs != 0 || q3.calls != 0 {
		t.Fatalf("a queryid with only a break is a flagged zero row, got %+v", q3)
	}
	if got := sumIntervals(nil); len(got) != 0 {
		t.Fatalf("no intervals: %+v", got)
	}
}

func TestLastDueHour_FiveMinutesPastTheHour(t *testing.T) {
	h := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{h.Add(4*time.Minute + 59*time.Second), h.Add(-2 * time.Hour)},
		{h.Add(5 * time.Minute), h.Add(-time.Hour)},
		{h.Add(59 * time.Minute), h.Add(-time.Hour)},
	} {
		if got := lastDueHour(c.now); !got.Equal(c.want) {
			t.Errorf("lastDueHour(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestPassHours_BackfillCapAndSteadyReRoll(t *testing.T) {
	last := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	back := passHours(last.Add(-100*time.Hour), last)
	if len(back) != backfillHoursPerPass || !back[0].Equal(last.Add(-100*time.Hour)) {
		t.Fatalf("backfill pass = %d hours from %v", len(back), back[0])
	}
	steady := passHours(last, last)
	if len(steady) != 2 || !steady[0].Equal(last.Add(-time.Hour)) || !steady[1].Equal(last) {
		t.Fatalf("steady pass = %v, want h-2 then h-1", steady)
	}
	if caught := passHours(last.Add(time.Hour), last); len(caught) != 0 {
		t.Fatalf("caught up: %v, want nothing", caught)
	}
}

func TestRollupMetrics_CountByResultAndLag(t *testing.T) {
	noteRollupHour("metrics_db", "ok")
	noteRollupHour("metrics_db", "ok")
	noteRollupHour("metrics_db", "failed")
	setRollupLag("metrics_db", 3)
	if rollupCountOf("metrics_db", "ok") != 2 || rollupCountOf("metrics_db", "failed") != 1 ||
		rollupCountOf("metrics_db", "empty") != 0 {
		t.Fatalf("counts = %+v", RollupHourCounts())
	}
	if rollupLagOf("metrics_db") != 3 {
		t.Fatalf("lags = %+v", RollupLags())
	}
}
