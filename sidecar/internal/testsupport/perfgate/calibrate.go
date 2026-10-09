package perfgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The reference runner is the one the timing budgets were set on: a
// GitHub ubuntu-latest runner with the gate's clean PG17 container. Its
// workload times are the median of perfgate.yml runs (2026-10-09).
const (
	ReferenceCPUMs = 100.0
	ReferenceDBMs  = 100.0
	// MaxCalibrationFactor bounds how much a slow runner loosens the
	// timing budgets, so a regression cannot hide behind one.
	MaxCalibrationFactor = 1.5
	calibrationRuns      = 5
)

// Calibration is the runner's speed against the reference runner: the
// best of calibrationRuns timings of each fixed workload, and the factors
// the timing budgets are scaled by.
type Calibration struct {
	CPUMs, DBMs         float64
	CPUFactor, DBFactor float64
	Known               bool
}

// NewCalibration turns measured workload times into budget factors:
// measured/reference, never below 1 (a fast runner is held to the shipped
// budgets), at most MaxCalibrationFactor. An unknown time scales nothing.
func NewCalibration(cpuMs, dbMs float64) Calibration {
	return Calibration{CPUMs: cpuMs, DBMs: dbMs, Known: true,
		CPUFactor: calibrationFactor(cpuMs, ReferenceCPUMs),
		DBFactor:  calibrationFactor(dbMs, ReferenceDBMs)}
}

func calibrationFactor(measured, reference float64) float64 {
	if !(measured > 0) || math.IsInf(measured, 0) {
		return 1
	}
	return math.Min(math.Max(measured/reference, 1), MaxCalibrationFactor)
}

// Calibrated returns b with its timing budgets scaled: database times by
// the SQL workload's factor, the sidecar's CPU by the CPU workload's.
// Count budgets and exemptions are unchanged.
func (b Budgets) Calibrated(c Calibration) Budgets {
	if !c.Known {
		return b
	}
	b.StatementMeanMs *= c.DBFactor
	b.CycleDBTimeMs *= c.DBFactor
	b.CatalogStatementMaxMs *= c.DBFactor
	b.EndpointMaxMs *= c.DBFactor
	b.SidecarCPUMsPerCycle *= c.CPUFactor
	return b
}

// CalibrateCPU times a fixed CPU workload shaped like the sidecar's own
// (JSON documents and regular expressions); the best of calibrationRuns.
func CalibrateCPU() float64 {
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

// calibrationSQL is a fixed server-side workload: aggregation over
// generated rows, no tables, no JIT.
const calibrationSQL = `SELECT count(*) FROM generate_series(1, 1500000) g
WHERE g % 7 = 3`

// CalibrateDB times calibrationSQL on the gate's server; the best of
// calibrationRuns.
func CalibrateDB(ctx context.Context, pool *pgxpool.Pool) (float64, error) {
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

func timeCalibrationQuery(ctx context.Context, pool *pgxpool.Pool) (float64, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('jit', 'off', true)"); err != nil {
		return 0, err
	}
	var n int64
	start := time.Now()
	if err := tx.QueryRow(ctx, calibrationSQL).Scan(&n); err != nil {
		return 0, err
	}
	return float64(time.Since(start).Microseconds()) / 1000, nil
}
