package executor

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// Outcome metrics: the signal a config change is meant to move. A change
// is credited 'success' only when its metric moved the right way over the
// monitor window; anything unmeasurable is 'unverifiable' (G-P0-1).
const (
	metricTempSpills = "temp_spills" // work_mem: temp files per second
	metricDeadTuples = "dead_tuples" // autovacuum knobs: dead-tuple ratio
	metricHotUpdates = "hot_updates" // fillfactor: share of HOT updates
)

const (
	minExpectedSpills = 3    // fewer expected temp files cannot show a drop
	spillDropFactor   = 0.5  // observed spills must be at most half of expected
	deadRatioFactor   = 0.8  // dead-tuple ratio must fall by at least 20%
	minWindowUpdates  = 100  // updates needed to judge a HOT ratio
	hotRatioGain      = 0.05 // HOT share must rise by 5 points
	judgeEpsilon      = 1e-9
)

// outcomeBaseline is the metric's counters at apply time, stored in
// before_state so a resumed monitor judges the same baseline.
type outcomeBaseline struct {
	Metric   string             `json:"metric"`
	Table    string             `json:"table,omitempty"`
	At       time.Time          `json:"at"`
	Counters map[string]float64 `json:"counters"`
}

// outcomeMetricFor names the metric a change targets ("" = none).
func outcomeMetricFor(c *configChange) (metric, table string) {
	if c == nil {
		return "", ""
	}
	if c.kind == configKindGUC {
		switch {
		case c.guc.Name == "work_mem":
			return metricTempSpills, ""
		case vacuumKnob(c.guc.Name):
			return metricDeadTuples, ""
		}
		return "", ""
	}
	for _, opt := range c.table.Options {
		base, _ := pgconf.ReloptionBaseKey(opt.Key)
		if vacuumKnob(base) {
			return metricDeadTuples, c.table.Table
		}
		if base == "fillfactor" {
			return metricHotUpdates, c.table.Table
		}
	}
	return "", ""
}

func vacuumKnob(name string) bool {
	return strings.HasPrefix(name, "autovacuum_vacuum_")
}

// outcomeQueries read each metric's counters; $1 is the table, if any.
var outcomeQueries = map[string]string{
	metricTempSpills: `/* pg_sage */ SELECT temp_files::float8, temp_files::float8 /
		GREATEST(EXTRACT(EPOCH FROM now() - COALESCE(stats_reset,
		pg_postmaster_start_time())), 1)::float8
		FROM pg_stat_database WHERE datname = current_database()`,
	metricDeadTuples + ":table": `/* pg_sage */ SELECT n_dead_tup::float8,
		n_live_tup::float8, autovacuum_count::float8
		FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
	metricDeadTuples: `/* pg_sage */ SELECT COALESCE(sum(n_dead_tup), 0)::float8,
		COALESCE(sum(n_live_tup), 0)::float8, COALESCE(sum(autovacuum_count), 0)::float8
		FROM pg_stat_user_tables`,
	metricHotUpdates + ":table": `/* pg_sage */ SELECT n_tup_upd::float8,
		n_tup_hot_upd::float8 FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
}

var outcomeColumns = map[string][]string{
	metricTempSpills: {"temp_files", "rate_per_sec"},
	metricDeadTuples: {"n_dead_tup", "n_live_tup", "autovacuum_count"},
	metricHotUpdates: {"n_tup_upd", "n_tup_hot_upd"},
}

// readOutcomeCounters reads the metric's counters now.
func readOutcomeCounters(
	ctx context.Context, pool *pgxpool.Pool, metric, table string,
) (map[string]float64, error) {
	key, args := metric, []any{}
	if table != "" {
		key, args = metric+":table", []any{table}
	}
	query, ok := outcomeQueries[key]
	cols := outcomeColumns[metric]
	if !ok {
		return nil, fmt.Errorf("no counters for metric %q", key)
	}
	values := make([]float64, len(cols))
	dest := make([]any, len(cols))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := pool.QueryRow(ctx, query, args...).Scan(dest...); err != nil {
		return nil, fmt.Errorf("read %s counters: %w", key, err)
	}
	out := make(map[string]float64, len(cols))
	for i, col := range cols {
		out[col] = values[i]
	}
	return out, nil
}

// captureOutcomeBaseline records the metric's counters at apply time.
func captureOutcomeBaseline(
	ctx context.Context, pool *pgxpool.Pool, metric, table string,
) (*outcomeBaseline, error) {
	counters, err := readOutcomeCounters(ctx, pool, metric, table)
	if err != nil {
		return nil, err
	}
	return &outcomeBaseline{Metric: metric, Table: table, At: time.Now().UTC(),
		Counters: counters}, nil
}

// judgeOutcome decides whether the metric moved enough to credit.
func judgeOutcome(b outcomeBaseline, now map[string]float64, at time.Time) (bool, string) {
	switch b.Metric {
	case metricTempSpills:
		return judgeTempSpills(b.Counters, now, at.Sub(b.At))
	case metricDeadTuples:
		return judgeDeadTuples(b.Counters, now)
	case metricHotUpdates:
		return judgeHotUpdates(b.Counters, now)
	}
	return false, fmt.Sprintf("no targeted metric %q to verify the change", b.Metric)
}

func counters(m map[string]float64, keys ...string) ([]float64, bool) {
	out := make([]float64, len(keys))
	for i, k := range keys {
		v, ok := m[k]
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// judgeTempSpills credits a change when the window saw at most half the
// temp files the pre-change rate predicts.
func judgeTempSpills(before, after map[string]float64, elapsed time.Duration) (bool, string) {
	b, ok1 := counters(before, "temp_files", "rate_per_sec")
	a, ok2 := counters(after, "temp_files")
	if !ok1 || !ok2 {
		return false, "temp-file counters missing; not credited"
	}
	if b[0] <= 0 || b[1] <= 0 {
		return false, "no temp-file spills before the change; nothing to improve"
	}
	if a[0] < b[0] {
		return false, "statistics were reset during the window; not credited"
	}
	expected := b[1] * elapsed.Seconds()
	if expected < minExpectedSpills-judgeEpsilon {
		return false, fmt.Sprintf("too few spills expected in the window (%.1f) to judge",
			expected)
	}
	observed := a[0] - b[0]
	if observed <= spillDropFactor*expected+judgeEpsilon {
		return true, fmt.Sprintf("temp files fell: %.0f observed vs %.1f expected", observed,
			expected)
	}
	return false, fmt.Sprintf("temp files did not fall: %.0f observed vs %.1f expected",
		observed, expected)
}

// judgeDeadTuples credits an autovacuum change when autovacuum ran and
// the dead-tuple ratio fell by at least 20%.
func judgeDeadTuples(before, after map[string]float64) (bool, string) {
	b, ok1 := counters(before, "n_dead_tup", "n_live_tup", "autovacuum_count")
	a, ok2 := counters(after, "n_dead_tup", "n_live_tup", "autovacuum_count")
	if !ok1 || !ok2 {
		return false, "dead-tuple counters missing; not credited"
	}
	if b[0] <= 0 || b[0]+b[1] <= 0 {
		return false, "no dead tuples before the change; nothing to improve"
	}
	if a[2] <= b[2] {
		return false, "autovacuum did not run on the target during the window"
	}
	r0 := b[0] / (b[0] + b[1])
	r1 := 0.0
	if a[0]+a[1] > 0 {
		r1 = a[0] / (a[0] + a[1])
	}
	if r1 <= deadRatioFactor*r0+judgeEpsilon {
		return true, fmt.Sprintf("dead-tuple ratio fell from %.3f to %.3f", r0, r1)
	}
	return false, fmt.Sprintf("dead-tuple ratio did not fall enough (%.3f -> %.3f)", r0, r1)
}

// judgeHotUpdates credits a fillfactor change when the window's share of
// HOT updates beats the lifetime share by 5 points.
func judgeHotUpdates(before, after map[string]float64) (bool, string) {
	b, ok1 := counters(before, "n_tup_upd", "n_tup_hot_upd")
	a, ok2 := counters(after, "n_tup_upd", "n_tup_hot_upd")
	if !ok1 || !ok2 {
		return false, "update counters missing; not credited"
	}
	updates := a[0] - b[0]
	if updates < minWindowUpdates {
		return false, fmt.Sprintf("too few updates in the window (%.0f) to judge", updates)
	}
	base := 0.0
	if b[0] > 0 {
		base = b[1] / b[0]
	}
	window := (a[1] - b[1]) / updates
	if window-base >= hotRatioGain-judgeEpsilon {
		return true, fmt.Sprintf("HOT update share rose from %.3f to %.3f", base, window)
	}
	return false, fmt.Sprintf("HOT update share did not rise enough (%.3f -> %.3f)",
		base, math.Max(window, 0))
}
