package perfgate

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCalibrateCPUMeasuresTheFixedWorkload(t *testing.T) {
	ms := calibrateCPU()
	if ms <= 0 || ms > 60000 {
		t.Fatalf("cpu calibration = %v ms", ms)
	}
}

func TestCalibrateDBMeasuresTheFixedQuery(t *testing.T) {
	pool, ctx := livePool(t)
	ms, err := calibrateDB(ctx, pool)
	if err != nil || ms <= 0 || ms > 60000 {
		t.Fatalf("db calibration = %v ms, %v", ms, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := calibrateDB(canceled, pool); err == nil ||
		!strings.Contains(err.Error(), "calibration") {
		t.Fatalf("canceled calibration: %v, want an error naming calibration", err)
	}
	if _, err := calibrateDB(ctx, nil); err == nil ||
		!strings.Contains(err.Error(), "no connection pool") {
		t.Fatalf("calibration without a pool: %v, want an error saying so", err)
	}
}

// Calibrate measures both workloads and records the runner's type.
func TestCalibrateMeasuresThisRunner(t *testing.T) {
	pool, ctx := livePool(t)
	c, err := Calibrate(ctx, pool)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if !c.Known || !(c.CPUMs > 0) || !(c.DBMs > 0) || c.CPUs != runtime.NumCPU() {
		t.Fatalf("calibration = %+v, want both times and %d CPUs", c, runtime.NumCPU())
	}
	for _, f := range []float64{c.CPUFactor, c.DBFactor} {
		if f < MinCalibrationFactor || f > MaxCalibrationFactor {
			t.Fatalf("factor %v outside the clamp: %+v", f, c)
		}
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" && c.CPUModel == "" {
		t.Fatalf("no CPU model on linux/amd64, whose /proc/cpuinfo names one: %+v", c)
	}
	if _, err := Calibrate(ctx, nil); err == nil ||
		!strings.Contains(err.Error(), "calibration") {
		t.Fatalf("calibrate without a pool: %v, want a calibration error", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := Calibrate(canceled, pool); !errors.Is(err, context.Canceled) ||
		got.Known {
		t.Fatalf("canceled calibrate: %+v, %v; want context.Canceled", got, err)
	}
}

// The SQL workload must time the server's CPU, not its disk: at the default
// 4MB work_mem generate_series's 1.5 million rows spilled about 20 MB to
// temp files. pg_stat_statements must show that the calibration's own runs
// wrote and read no temp block; the same statement run at 4MB must then
// show the spill, so the zero is not a counter that never counts.
func TestCalibrationSQLRunsInMemory(t *testing.T) {
	pool, ctx := livePool(t)
	if err := Prepare(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := calibrateDB(ctx, pool); err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	calls, written, read := calibrationTempBlocks(t, ctx, pool)
	if calls != calibrationRuns || written != 0 || read != 0 {
		t.Fatalf("calibration: %d calls, %d temp blocks written, %d read; want %d calls "+
			"and no temp block", calls, written, read, calibrationRuns)
	}
	runCalibrationSQLAt4MB(t, ctx, pool)
	calls, written, read = calibrationTempBlocks(t, ctx, pool)
	if calls != calibrationRuns+1 || written == 0 || read == 0 {
		t.Fatalf("at work_mem 4MB: %d calls, %d temp blocks written, %d read; want "+
			"the spill counted", calls, written, read)
	}
}

func calibrationTempBlocks(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) (calls, written, read int64) {
	t.Helper()
	err := pool.QueryRow(ctx, `/* `+HarnessTag+` */
		SELECT COALESCE(sum(s.calls), 0)::int8, COALESCE(sum(s.temp_blks_written), 0)::int8,
		       COALESCE(sum(s.temp_blks_read), 0)::int8
		FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database() AND strpos(s.query, $1) > 0`,
		calibrationTag).Scan(&calls, &written, &read)
	if err != nil {
		t.Fatalf("read the calibration's pg_stat_statements entry: %v", err)
	}
	return calls, written, read
}

func runCalibrationSQLAt4MB(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL work_mem = '4MB'"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SET LOCAL jit = off"); err != nil {
				return err
			}
			var n int64
			return tx.QueryRow(ctx, calibrationSQL).Scan(&n)
		})
	if err != nil {
		t.Fatalf("calibration SQL at work_mem 4MB: %v", err)
	}
}
