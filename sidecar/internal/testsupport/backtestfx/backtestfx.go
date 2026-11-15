// Package backtestfx writes a verifier back-test fixture (a Source, built
// from a backtest.Memory) into a database as the collector would have: one
// queries, tables and system snapshot per cycle through snapstore,
// cumulative counters, the statistics epoch changing at each of the
// fixture's breaks, then the hourly rollup rebuilt from the snapshots.
// Tests only. It does not import the backtest package, whose own tests
// use it.
package backtestfx

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// Statement is one statement of the fixture and the tables (schema.name)
// it names.
type Statement struct {
	QueryID int64
	Tables  []string
}

// Cycle is one statement's counter deltas over the cycle ending at To.
type Cycle struct {
	To                  time.Time
	Calls, Blocks, Rows int64
	Ms                  float64
}

// Source is a fixture: its first collection and last cycle end, its
// statements, each statement's cycles (oldest first, one per collection
// after First) and the cycle ends with an epoch break.
type Source struct {
	First, Last time.Time
	Statements  []Statement
	Cycles      map[int64][]Cycle
	Breaks      []time.Time
}

// counters is one statement's cumulative counters.
type counters struct {
	calls, blocks, rows int64
	ms                  float64
}

// Persist writes src's cycles, one every cycle from its first collection,
// into db's history and rebuilds the hourly rollup over rollupDays.
func Persist(t testing.TB, db snapstore.Beginner, src Source, cycle time.Duration,
	rollupDays int) {
	t.Helper()
	ctx := context.Background()
	if src.Last.IsZero() || cycle <= 0 {
		t.Fatal("backtestfx: the fixture has no cycles")
	}
	tables := tablesDoc(t, src.Statements)
	w := snapstore.NewWriter()
	sum := map[int64]*counters{}
	epoch := src.First.Add(-time.Hour)
	for i, at := 0, src.First; !at.After(src.Last); i, at = i+1, at.Add(cycle) {
		if slices.ContainsFunc(src.Breaks, at.Equal) {
			epoch = at
		}
		rows := []snapstore.Row{
			{Category: "queries", Data: queriesDoc(t, src, sum, at, i)},
			{Category: "tables", Data: tables},
			{Category: "system", Data: mustJSON(t, map[string]any{"active_backends": 1,
				"statements_epoch": epoch.UTC().Format(time.RFC3339Nano)})},
		}
		if err := w.Persist(ctx, db, at, rows); err != nil {
			t.Fatalf("backtestfx: persist the cycle at %s: %v", at, err)
		}
	}
	if _, err := querystore.RebuildHourly(ctx, db, rollupDays); err != nil {
		t.Fatalf("backtestfx: rebuild the hourly rollup: %v", err)
	}
}

// queriesDoc is the queries snapshot at at: each statement's counters
// with the cycle ending at at added (the i-th cycle; none at i == 0). The
// epoch change at a break makes that cycle count nothing, as in Memory.
func queriesDoc(t testing.TB, src Source, sum map[int64]*counters, at time.Time,
	i int) []byte {
	docs := make([]map[string]any, 0, len(src.Statements))
	for _, s := range src.Statements {
		c := sum[s.QueryID]
		if c == nil {
			c = &counters{}
			sum[s.QueryID] = c
		}
		if i > 0 {
			iv := src.Cycles[s.QueryID][i-1]
			if !iv.To.Equal(at) {
				t.Fatalf("backtestfx: cycle %d of query %d ends at %s, want %s", i,
					s.QueryID, iv.To, at)
			}
			c.calls, c.blocks, c.rows = c.calls+iv.Calls, c.blocks+iv.Blocks, c.rows+iv.Rows
			c.ms += iv.Ms
		}
		docs = append(docs, map[string]any{"queryid": s.QueryID, "query": queryText(s),
			"calls": c.calls, "total_exec_time": c.ms, "mean_exec_time": 0, "rows": c.rows,
			"shared_blks_hit": c.blocks, "shared_blks_read": 0, "temp_blks_read": 0,
			"temp_blks_written": 0, "wal_bytes": 0})
	}
	return mustJSON(t, docs)
}

// queryText names the statement's tables, unqualified.
func queryText(s Statement) string {
	names := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		names = append(names, t[strings.LastIndex(t, ".")+1:])
	}
	return fmt.Sprintf("SELECT count(*) FROM %s WHERE id = $1 /* q%d */",
		strings.Join(names, ", "), s.QueryID)
}

func tablesDoc(t testing.TB, stmts []Statement) []byte {
	var docs []map[string]any
	seen := map[string]bool{}
	for _, s := range stmts {
		for _, table := range s.Tables {
			if seen[table] {
				continue
			}
			seen[table] = true
			schema, name, _ := strings.Cut(table, ".")
			docs = append(docs, map[string]any{"schemaname": schema, "relname": name})
		}
	}
	return mustJSON(t, docs)
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("backtestfx: %v", err)
	}
	return b
}
