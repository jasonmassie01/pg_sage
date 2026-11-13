package gameday

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// benches-12: a game day binds its customer program to the monitored
// database's hottest table before it creates the clone, passes the
// binding to the fault programs, and records what it exercised. A skipped
// customer program finishes completed_unscored; a failed one fails the
// game day.

type fakeSnapshots struct{ latest, previous *collector.Snapshot }

func (s fakeSnapshots) LatestSnapshot() *collector.Snapshot   { return s.latest }
func (s fakeSnapshots) PreviousSnapshot() *collector.Snapshot { return s.previous }

func hotOrders() fakeSnapshots {
	return fakeSnapshots{
		previous: snap(tableActivity{schema: "public", name: "orders", upd: 10}),
		latest:   snap(tableActivity{schema: "public", name: "orders", upd: 90})}
}

func (f *gdFixture) customerRunner(provider string, programs *bool,
	snaps TableSnapshots) *Runner {
	f.t.Helper()
	r, err := NewRunner(Config{Database: f.database, Provider: provider,
		Families: []string{"lock_blocking"}, Interval: 7 * 24 * time.Hour,
		Now: func() time.Time { return f.now }, CustomerPrograms: programs,
		Snapshots: snaps, Warn: func(string, ...any) {}}, f.provider, f.faults, f.ledger,
		f.store)
	if err != nil {
		f.t.Fatalf("runner: %v", err)
	}
	return r
}

const boundRuns = `{"scenario": "lock-hot-row", "arm": "causal-graph"},
	{"scenario": "lock_customer_hot_table", "arm": "causal-graph"}`

// benchReport is a report the ledger accepts, with runs (JSON objects).
func benchReport(at time.Time, runs string) []byte {
	return []byte(fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1",
		"generated_at": %q, "gated_arms": ["causal-graph"], "cells": [
		{"arm": "causal-graph", "family": "lock_blocking", "runs": 2,
		 "safe_pass": {"k": 1, "n": 1}, "top1": {"k": 1, "n": 1},
		 "mechanism_precision": 1, "forbidden_actions": 0}],
		"runs": [%s]}`, at.UTC().Format(time.RFC3339), runs))
}

// State transitions: running -> completed with exercised recorded -> the
// clone destroyed; the stored row reads back the same record.
func TestGameDayBindsTheHotTableAndRecordsWhatItExercised(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = benchReport(f.now, boundRuns)
	gd, err := f.customerRunner("dle", nil, hotOrders()).Run(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gd.Status != StatusCompleted || len(f.provider.destroyed) != 1 {
		t.Fatalf("game day = %+v, destroyed %d", gd, len(f.provider.destroyed))
	}
	if len(f.faults.bindings) != 1 || len(f.faults.bindings[0]) != 1 ||
		f.faults.bindings[0][0] != boundOrders() {
		t.Fatalf("fault programs got bindings %+v, want public.orders", f.faults.bindings)
	}
	x := gd.Exercised
	if x == nil || x.Provider != "dle" || !x.CustomerObjects || len(x.Scenarios) != 2 ||
		x.Scenarios[1].ID != ProgramCustomerHotTable ||
		x.Scenarios[1].Binding != `"public"."orders"` || x.Scenarios[1].Scored != 1 {
		t.Fatalf("exercised = %+v", x)
	}
	stored, err := f.store.Latest(f.ctx, f.database)
	if err != nil || stored == nil || stored.Exercised == nil {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if got := stored.Exercised.Scenarios[1]; got != x.Scenarios[1] {
		t.Fatalf("stored customer scenario = %+v, want %+v", got, x.Scenarios[1])
	}
	// CHECK-01, scoped to this test's database.
	if got := checkScenarios(f); !strings.Contains(got, `"id": "lock_customer_hot_table"`) ||
		!strings.Contains(got, `"binding": "\"public\".\"orders\""`) {
		t.Fatalf("exercised->'scenarios' = %s, want the bound customer program", got)
	}
}

// checkScenarios runs the acceptance query on f's database's newest game
// day.
func checkScenarios(f *gdFixture) string {
	f.t.Helper()
	var got string
	if err := testPool(f.t).QueryRow(f.ctx, `SELECT (exercised->'scenarios')::text
		FROM sage.sre_game_days WHERE database_name = $1
		ORDER BY started_at DESC LIMIT 1`, f.database).Scan(&got); err != nil {
		f.t.Fatalf("acceptance query: %v", err)
	}
	return got
}

// The local provider, customer programs switched off, or no snapshot: the
// fault programs get a skip binding, never a table, and a run whose
// customer program skipped finishes completed_unscored.
func TestGameDayCustomerSkipFinishesUnscored(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name, provider string
		programs       *bool
		snaps          TableSnapshots
		want           string
	}{
		{"local provider", "local", &on, hotOrders(), SkipLocalProvider},
		{"customer programs off", "dle", &off, hotOrders(), SkipProgramsOff},
		{"no snapshot source", "dle", nil, nil, SkipNoSnapshot},
		{"one snapshot", "dle", nil, fakeSnapshots{latest: hotOrders().latest},
			SkipOneSnapshot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGDFixture(t)
			f.faults.report = benchReport(f.now, `
				{"scenario": "lock-hot-row", "arm": "causal-graph"},
				{"scenario": "lock_customer_hot_table", "arm": "causal-graph",
				 "skipped": "`+tc.want+`"}`)
			gd, err := f.customerRunner(tc.provider, tc.programs, tc.snaps).Run(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := f.faults.bindings[0]
			if len(got) != 1 || got[0].Bound() || got[0].Skip != tc.want {
				t.Fatalf("bindings = %+v, want skip %q", got, tc.want)
			}
			if gd.Status != StatusCompletedUnscored {
				t.Fatalf("status = %s, want completed_unscored", gd.Status)
			}
			sc := gd.Exercised.Scenarios[1]
			if sc.Skip != tc.want || sc.Binding != "" {
				t.Fatalf("customer scenario = %+v, want skip %q", sc, tc.want)
			}
			// CHECK-02, scoped to this test's database.
			if got := checkScenarios(f); !strings.Contains(got, tc.want) ||
				strings.Contains(got, "orders") {
				t.Fatalf("exercised->'scenarios' = %s, want skip %q", got, tc.want)
			}
			if gd.Exercised.CustomerObjects != (tc.provider != "local") {
				t.Fatalf("customer_objects = %v on %s", gd.Exercised.CustomerObjects,
					tc.provider)
			}
		})
	}
}

// Error propagation: a customer program that failed with 42501 fails the
// game day (never completed), with the SQLSTATE in its error, and the
// clone is still destroyed.
func TestGameDayCustomerErrorFailsTheGameDay(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = benchReport(f.now, `{"scenario": "lock-hot-row", "arm": "causal-graph"},
		{"scenario": "lock_customer_hot_table", "arm": "causal-graph",
		 "error": "inject: holder lock on the bound table: SQLSTATE 42501"}`)
	gd, err := f.customerRunner("dle", nil, hotOrders()).Run(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE 42501") {
		t.Fatalf("err = %v, want the SQLSTATE", err)
	}
	if gd.Status != StatusFailed || !strings.Contains(gd.Error, "42501") ||
		len(f.provider.destroyed) != 1 {
		t.Fatalf("game day = %+v, destroyed %d", gd, len(f.provider.destroyed))
	}
	stored, serr := f.store.Latest(f.ctx, f.database)
	if serr != nil || stored.Status != StatusFailed || stored.Exercised == nil ||
		stored.Exercised.Scenarios[1].Error == "" {
		t.Fatalf("stored = %+v, %v", stored, serr)
	}
}

// A run that fails before any report still records its binding, so the
// row says what the run was about to exercise.
func TestGameDayFaultFailureKeepsTheBinding(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report, f.faults.err = nil, errors.New("connect clone: refused")
	gd, err := f.customerRunner("dle", nil, hotOrders()).Run(f.ctx)
	if err == nil || gd.Status != StatusFailed {
		t.Fatalf("game day = %+v, %v; want failed", gd, err)
	}
	if gd.Exercised == nil || len(gd.Exercised.Scenarios) != 1 ||
		gd.Exercised.Scenarios[0].BindingHash != boundOrders().Hash() {
		t.Fatalf("exercised = %+v, want the binding kept", gd.Exercised)
	}
}

// Concurrent access: a database's game days take turns (one runner per
// database, benches-01); overlapping requests are refused before anything
// is recorded or bound, and only one fault run happens.
func TestGameDaysOnOneDatabaseDoNotOverlap(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = benchReport(f.now, boundRuns)
	f.faults.block = make(chan struct{})
	r := f.customerRunner("dle", nil, hotOrders())
	if err := r.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = r.Run(f.ctx)
			} else {
				errs[i] = r.Start(f.ctx)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrRunning) {
			t.Fatalf("overlapping run %d: err = %v, want ErrRunning", i, err)
		}
	}
	close(f.faults.block)
	waitIdle(t, r)
	f.faults.mu.Lock()
	defer f.faults.mu.Unlock()
	if len(f.faults.bindings) != 1 {
		t.Fatalf("%d fault runs, want 1", len(f.faults.bindings))
	}
	rows, err := f.store.List(f.ctx, f.database, 10)
	if err != nil || len(rows) != 1 || rows[0].Status != StatusCompleted {
		t.Fatalf("rows = %+v, %v; want one completed game day", rows, err)
	}
}
