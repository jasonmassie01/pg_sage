package perfgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Calibrate measures this runner against the reference runner, before the
// fixture loads it: the SQL workload on the gate's server, the CPU
// workload in this process, and the runner's type.
func Calibrate(ctx context.Context, pool *pgxpool.Pool) (Calibration, error) {
	db, err := calibrateDB(ctx, pool)
	if err != nil {
		return Calibration{}, err
	}
	model, err := cpuModelFrom(cpuinfoPath)
	if err != nil {
		return Calibration{}, err
	}
	c, err := NewCalibration(calibrateCPU(), db)
	if err != nil {
		return Calibration{}, err
	}
	c.CPUModel, c.CPUs = model, runtime.NumCPU()
	return c, nil
}

// calibrateCPU times a fixed CPU workload shaped like the sidecar's own
// (JSON documents and regular expressions); the best of calibrationRuns.
func calibrateCPU() float64 {
	doc := calibrationDocument()
	best := math.Inf(1)
	for i := 0; i < calibrationRuns; i++ {
		start := time.Now()
		cpuWorkload(doc)
		best = math.Min(best, float64(time.Since(start).Microseconds())/1000)
	}
	return best
}

var calibrationPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func calibrationDocument() []byte {
	items := make([]map[string]any, 4000)
	for i := range items {
		items[i] = map[string]any{"schemaname": "app", "relname": fmt.Sprintf("t_%d", i),
			"n_live_tup": i * 31, "seq_scan": i % 7, "note": strings.Repeat("x", 16)}
	}
	doc, _ := json.Marshal(items)
	return doc
}

func cpuWorkload(doc []byte) {
	for round := 0; round < 4; round++ {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(doc, &items); err != nil {
			panic(err) // a fixed document: cannot fail
		}
		for _, item := range items {
			for _, v := range item {
				calibrationPattern.Match(v)
			}
		}
		if _, err := json.Marshal(items); err != nil {
			panic(err)
		}
	}
}

// calibrationTag marks the SQL workload as a harness statement, which the
// gate never charges to pg_sage.
const calibrationTag = HarnessTag + " calibration"

// calibrationSQL is a fixed server-side workload: aggregation over
// generated rows, no tables. calibrationSettings keep it on the server's
// CPU: JIT off, and work_mem room for generate_series's 1.5 million rows.
// At the default 4MB they spilled 2564 temp blocks (20 MB), so the
// workload timed the disk too; they need about 88MB on PostgreSQL 14,
// 72MB on 17 and 64MB on 18.
const calibrationSQL = `/* ` + calibrationTag + ` */ SELECT count(*)
FROM generate_series(1, 1500000) g WHERE g % 7 = 3`

var calibrationSettings = []string{"SET LOCAL jit = off", "SET LOCAL work_mem = '256MB'"}

// calibrateDB times calibrationSQL on the gate's server; the best of
// calibrationRuns.
func calibrateDB(ctx context.Context, pool *pgxpool.Pool) (float64, error) {
	if pool == nil {
		return 0, errors.New("perfgate calibration: no connection pool")
	}
	best := math.Inf(1)
	for i := 0; i < calibrationRuns; i++ {
		ms, err := timeCalibrationQuery(ctx, pool)
		if err != nil {
			return 0, fmt.Errorf("perfgate calibration: SQL workload: %w", err)
		}
		best = math.Min(best, ms)
	}
	return best, nil
}

// timeCalibrationQuery runs calibrationSQL once, in a read-only
// transaction of its own that its SET LOCAL settings end with.
func timeCalibrationQuery(ctx context.Context, pool *pgxpool.Pool) (float64, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, set := range calibrationSettings {
		if _, err := tx.Exec(ctx, set); err != nil {
			return 0, fmt.Errorf("%s: %w", set, err)
		}
	}
	var n int64
	start := time.Now()
	if err := tx.QueryRow(ctx, calibrationSQL).Scan(&n); err != nil {
		return 0, err
	}
	return float64(time.Since(start).Microseconds()) / 1000, nil
}
