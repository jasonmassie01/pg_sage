// Package perfgate is pg_sage's performance gate: a large synthetic
// monitored database (catalog plus pre-seeded sage history), measurements
// of what pg_sage does to it (pg_stat_statements, per-table access and
// write counters, generic plans) and the budgets those measurements must
// meet. TestPerfGate in cmd/pg_sage_sidecar (build tag perfgate) runs the
// real runtime against the fixture and fails on any offender.
package perfgate

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Environment variables that size and time the gate.
const (
	EnvScale       = "PG_SAGE_PERF_SCALE"        // small (PR CI) or large (nightly)
	EnvTables      = "PG_SAGE_PERF_TABLES"       // total monitored tables
	EnvHistoryRows = "PG_SAGE_PERF_HISTORY_ROWS" // rows per growing sage table
	EnvInterval    = "PG_SAGE_PERF_INTERVAL"     // every component's interval
	EnvWarmup      = "PG_SAGE_PERF_WARMUP"       // startup phase, not budgeted per cycle
	EnvWindow      = "PG_SAGE_PERF_WINDOW"       // steady phase, budgeted per cycle
)

const (
	tablesPerSchema = 50
	cloneShare      = 0.4 // 40 of 100 schemas are identical clones
	maxIndexes      = 5   // the primary key and the four shapes in buildSchemaFn
	minHistoryRows  = 100
	hotTableCount   = 10
)

// Scale sizes the synthetic monitored database.
type Scale struct {
	Name            string
	Schemas         int // all user schemas, clones included
	CloneSchemas    int // identical copies of one schema (leaked clones)
	TablesPerSchema int
	IndexesPerTable int // the primary key included
	HistoryRows     int // rows per growing sage table
}

// SmallScale is the pull-request size: 500 tables, 20k history rows.
func SmallScale() Scale {
	return Scale{Name: "small", Schemas: 10, CloneSchemas: 4,
		TablesPerSchema: tablesPerSchema, IndexesPerTable: 3, HistoryRows: 20000}
}

// LargeScale is the nightly size: 5,000 tables, 15,000 indexes, 5,000
// sequences in 100 schemas (40 identical clones) and 150k rows of history
// in every growing sage table. 200k rows measured 1.77 GB and a 10.5 min
// build on PostgreSQL 17; 150k keeps the fixture under the 1.5 GB budget.
func LargeScale() Scale {
	return Scale{Name: "large", Schemas: 100, CloneSchemas: 40,
		TablesPerSchema: tablesPerSchema, IndexesPerTable: 3, HistoryRows: 150000}
}

// Tables, Indexes and Sequences count the user catalog (one bigserial
// sequence per table).
func (s Scale) Tables() int    { return s.Schemas * s.TablesPerSchema }
func (s Scale) Indexes() int   { return s.Tables() * s.IndexesPerTable }
func (s Scale) Sequences() int { return s.Tables() }

// HotTables are the populated tables the workload runs on.
func (s Scale) HotTables() []string {
	n := min(hotTableCount, s.TablesPerSchema)
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("%s.%s", appSchema(0), tableName(i)))
	}
	return out
}

// Validate rejects a scale the builder cannot produce.
func (s Scale) Validate() error {
	switch {
	case s.Schemas < 1 || s.TablesPerSchema < 1:
		return fmt.Errorf("perfgate scale needs at least one schema and table: %+v", s)
	case s.CloneSchemas < 0 || s.CloneSchemas >= s.Schemas:
		return fmt.Errorf("perfgate scale: %d clone schemas of %d", s.CloneSchemas, s.Schemas)
	case s.IndexesPerTable < 1 || s.IndexesPerTable > maxIndexes:
		return fmt.Errorf("perfgate scale: indexes per table must be 1..%d", maxIndexes)
	case s.HistoryRows < minHistoryRows:
		return fmt.Errorf("perfgate scale: history rows must be >= %d", minHistoryRows)
	}
	return nil
}

// ScaleFromEnv reads the scale; getenv is os.Getenv in the gate.
func ScaleFromEnv(getenv func(string) string) (Scale, error) {
	var s Scale
	switch name := strings.TrimSpace(getenv(EnvScale)); name {
	case "", "small":
		s = SmallScale()
	case "large":
		s = LargeScale()
	default:
		return Scale{}, fmt.Errorf("%s=%q: want small or large", EnvScale, name)
	}
	overridden := false
	if v := getenv(EnvTables); v != "" {
		n, err := positiveInt(EnvTables, v)
		if err != nil {
			return Scale{}, err
		}
		if n < tablesPerSchema {
			return Scale{}, fmt.Errorf("%s=%d: want at least %d", EnvTables, n, tablesPerSchema)
		}
		s.Schemas = n / tablesPerSchema
		s.CloneSchemas = int(float64(s.Schemas) * cloneShare)
		overridden = true
	}
	if v := getenv(EnvHistoryRows); v != "" {
		n, err := positiveInt(EnvHistoryRows, v)
		if err != nil {
			return Scale{}, err
		}
		if n < minHistoryRows {
			return Scale{}, fmt.Errorf("%s=%d: want at least %d", EnvHistoryRows, n,
				minHistoryRows)
		}
		s.HistoryRows = n
		overridden = true
	}
	if overridden {
		s.Name += "+override"
	}
	return s, s.Validate()
}

func positiveInt(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive integer", name, v)
	}
	return n, nil
}

// Timing compresses every periodic component to one interval and splits
// the run into a warmup phase and a steady phase.
type Timing struct {
	Interval time.Duration
	Warmup   time.Duration
	Window   time.Duration
}

// DefaultTiming: 15 s cycles, 45 s warmup, 90 s steady window (6 cycles).
func DefaultTiming() Timing {
	return Timing{Interval: 15 * time.Second, Warmup: 45 * time.Second,
		Window: 90 * time.Second}
}

// Cycles is the number of whole cycles in the steady window.
func (t Timing) Cycles() int { return int(t.Window / t.Interval) }

// TimingFromEnv reads the timing overrides.
func TimingFromEnv(getenv func(string) string) (Timing, error) {
	t := DefaultTiming()
	for _, f := range []struct {
		name string
		dst  *time.Duration
	}{{EnvInterval, &t.Interval}, {EnvWarmup, &t.Warmup}, {EnvWindow, &t.Window}} {
		v := strings.TrimSpace(getenv(f.name))
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Timing{}, fmt.Errorf("%s=%q: want a positive duration", f.name, v)
		}
		*f.dst = d
	}
	if t.Interval < time.Second {
		return Timing{}, fmt.Errorf("%s must be at least 1s", EnvInterval)
	}
	if t.Warmup < t.Interval || t.Window < t.Interval {
		return Timing{}, fmt.Errorf("%s and %s must each cover at least one %s",
			EnvWarmup, EnvWindow, EnvInterval)
	}
	return t, nil
}
