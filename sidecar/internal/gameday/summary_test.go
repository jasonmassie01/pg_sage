package gameday

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// benches-08: a game day whose fault programs all skipped (a snapshot
// clone without superuser, max_prepared_transactions = 0) used to read
// "completed" and look like a safety signal. Each run now carries what
// its report scored, and a run that scored nothing is completed_unscored.

// No concurrent access tests for summarize and scoredStatus: both are pure
// functions of their arguments and share no state.

// runsReport is a bench report with the fixture's cell and the given runs.
func runsReport(at time.Time, runs ...string) []byte {
	return []byte(fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1",
		"generated_at": %q, "gated_arms": ["causal-graph"], "cells": [
		{"arm": "causal-graph", "family": "lock_blocking", "runs": 11,
		 "safe_pass": {"k": 11, "n": 11}, "top1": {"k": 7, "n": 7},
		 "mechanism_precision": 1, "forbidden_actions": 0}], "runs": [%s]}`,
		at.UTC().Format(time.RFC3339), strings.Join(runs, ",")))
}

func scoredRun(scenario string) string {
	return fmt.Sprintf(`{"scenario": %q, "family": "lock_blocking", "arm": "causal-graph",
		"state": "diagnosed", "root": "lock_blocking", "probe_count": 3}`, scenario)
}

func skippedRun(scenario, reason string) string {
	return fmt.Sprintf(`{"scenario": %q, "family": "wal_retention", "arm": "causal-graph",
		"probe_count": 0, "skipped": %q}`, scenario, reason)
}

func erroredRun(scenario, msg string) string {
	return fmt.Sprintf(`{"scenario": %q, "family": "wal_retention", "arm": "causal-graph",
		"probe_count": 0, "error": %q}`, scenario, msg)
}

const (
	skipSuperuser = "needs superuser (CHECKPOINT)"
	skipPrepared  = "max_prepared_transactions is 0"
)

// Happy path: each run is counted once, skipped before errored.
func TestSummarizeCountsScoredSkippedAndErroredRuns(t *testing.T) {
	raw := runsReport(time.Now(), scoredRun("a"), scoredRun("b"),
		skippedRun("c", skipSuperuser), skippedRun("d", skipSuperuser),
		skippedRun("e", skipPrepared), erroredRun("f", "inject: connection reset"),
		`{"scenario": "g", "skipped": "needs superuser (CHECKPOINT)", "error": "recover"}`)
	got := summarize(raw)
	want := Summary{Scored: 2, Skipped: 4, Errored: 1, TopSkips: []SkipCount{
		{Reason: skipSuperuser, Count: 3}, {Reason: skipPrepared, Count: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

// Boundary: 7 distinct reasons keep the 5 most frequent, ties by reason.
func TestSummarizeKeepsTheTopFiveSkipReasons(t *testing.T) {
	counts := map[string]int{"r-a": 1, "r-b": 4, "r-c": 2, "r-d": 2, "r-e": 1, "r-f": 3,
		"r-g": 1}
	var runs []string
	for reason, n := range counts {
		for i := 0; i < n; i++ {
			runs = append(runs, skippedRun(fmt.Sprintf("%s-%d", reason, i), reason))
		}
	}
	got := summarize(runsReport(time.Now(), runs...))
	want := []SkipCount{{"r-b", 4}, {"r-f", 3}, {"r-c", 2}, {"r-d", 2}, {"r-a", 1}}
	if got.Skipped != 14 || got.Scored != 0 || !reflect.DeepEqual(got.TopSkips, want) {
		t.Fatalf("summary = %+v, want 14 skipped and top skips %+v", got, want)
	}
}

// Boundary: exactly five reasons are all kept; a reason longer than the
// cap is cut so one fault program cannot bloat the row.
func TestSummarizeKeepsFiveReasonsAndCutsALongOne(t *testing.T) {
	long := strings.Repeat("x", maxSkipReason+50)
	runs := []string{skippedRun("a", long)}
	for _, r := range []string{"r1", "r2", "r3", "r4"} {
		runs = append(runs, skippedRun(r, r))
	}
	got := summarize(runsReport(time.Now(), runs...))
	if len(got.TopSkips) != 5 {
		t.Fatalf("top skips = %+v, want all 5", got.TopSkips)
	}
	for _, s := range got.TopSkips {
		if len(s.Reason) > maxSkipReason {
			t.Fatalf("reason of %d bytes kept, cap is %d", len(s.Reason), maxSkipReason)
		}
	}
}

// Nil/empty/invalid: no runs, no report, or an unreadable one scores
// nothing (it never reads as scored).
func TestSummarizeOfNoRunsOrAnUnreadableReportScoresNothing(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":            nil,
		"empty object":   []byte(`{}`),
		"no runs":        runsReport(time.Now()),
		"null runs":      []byte(`{"runs": null}`),
		"runs not array": []byte(`{"runs": "all of them"}`),
		"malformed":      []byte(`{"runs": [`),
		"not json":       []byte(`completed`),
	} {
		got := summarize(raw)
		if got.Scored != 0 || got.Skipped != 0 || got.Errored != 0 ||
			got.TopSkips == nil || len(got.TopSkips) != 0 {
			t.Fatalf("%s: summary = %+v, want zero counts and empty top skips", name, got)
		}
	}
}

// State transitions: only a completed run is re-read by what it scored.
func TestScoredStatus(t *testing.T) {
	none, one := &Summary{Skipped: 3}, &Summary{Scored: 1, Skipped: 9}
	for _, c := range []struct {
		status string
		s      *Summary
		want   string
	}{
		{StatusCompleted, none, StatusCompletedUnscored},
		{StatusCompleted, &Summary{}, StatusCompletedUnscored},
		{StatusCompleted, nil, StatusCompletedUnscored},
		{StatusFailed, nil, StatusFailed},
		{StatusCompleted, one, StatusCompleted},
		{StatusFailed, none, StatusFailed},
		{StatusDestroyFailed, none, StatusDestroyFailed},
		{StatusDestroyFailed, one, StatusDestroyFailed},
	} {
		if got := scoredStatus(c.status, c.s); got != c.want {
			t.Fatalf("scoredStatus(%s, %+v) = %s, want %s", c.status, c.s, got, c.want)
		}
	}
}

// warnings captures a runner's WARN lines.
type warnings struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnings) warn(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string{}, w.lines...)
}

func (f *gdFixture) warnRunner(w *warnings) *Runner {
	f.t.Helper()
	r, err := NewRunner(Config{Database: f.database, Provider: "dle",
		Now: func() time.Time { return f.now }, Warn: w.warn},
		f.provider, f.faults, f.ledger, f.store)
	if err != nil {
		f.t.Fatalf("runner: %v", err)
	}
	return r
}

// State transition (real PostgreSQL): every run skipped gives
// completed_unscored, one WARN naming the reasons, the summary stored
// with the row, and the report still ingested.
func TestGameDayThatSkippedEveryRunIsCompletedUnscored(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = runsReport(f.now, skippedRun("a", skipSuperuser),
		skippedRun("b", skipSuperuser), skippedRun("c", skipPrepared))
	w := &warnings{}
	gd, err := f.warnRunner(w).Run(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := &Summary{Scored: 0, Skipped: 3, TopSkips: []SkipCount{
		{Reason: skipSuperuser, Count: 2}, {Reason: skipPrepared, Count: 1}}}
	if gd.Status != StatusCompletedUnscored || gd.EvalRunID == "" || gd.Error != "" ||
		!reflect.DeepEqual(gd.Summary, want) {
		t.Fatalf("game day = %+v (summary %+v)", gd, gd.Summary)
	}
	lines := w.all()
	if len(lines) != 1 || !strings.Contains(lines[0], gd.ID) ||
		!strings.Contains(lines[0], skipSuperuser) || !strings.Contains(lines[0], skipPrepared) ||
		!strings.Contains(lines[0], "0 of 3") {
		t.Fatalf("warnings = %q, want one naming the run and its skip reasons", lines)
	}
	list, err := f.store.List(f.ctx, f.database, 10)
	if err != nil || len(list) != 1 || list[0].Status != StatusCompletedUnscored ||
		!reflect.DeepEqual(list[0].Summary, want) {
		t.Fatalf("stored = %+v (%v)", list, err)
	}
}

// Boundary (real PostgreSQL): one scored run among skips is completed,
// with no warning.
func TestGameDayWithOneScoredRunIsCompleted(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = runsReport(f.now, scoredRun("a"), skippedRun("b", skipSuperuser))
	w := &warnings{}
	gd, err := f.warnRunner(w).Run(f.ctx)
	if err != nil || gd.Status != StatusCompleted || gd.Summary == nil ||
		gd.Summary.Scored != 1 || gd.Summary.Skipped != 1 {
		t.Fatalf("game day = %+v (%v)", gd, err)
	}
	if lines := w.all(); len(lines) != 0 {
		t.Fatalf("warnings = %q, want none", lines)
	}
}

// Nil/empty (real PostgreSQL): a report with zero runs scored nothing.
func TestGameDayWithZeroRunsIsCompletedUnscored(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = runsReport(f.now)
	w := &warnings{}
	gd, err := f.warnRunner(w).Run(f.ctx)
	if err != nil || gd.Status != StatusCompletedUnscored || gd.Summary == nil ||
		gd.Summary.Scored != 0 || gd.EvalRunID == "" || len(w.all()) != 1 {
		t.Fatalf("game day = %+v (%v), warnings %q", gd, err, w.all())
	}
}

// Error propagation (real PostgreSQL): a malformed report is an ingest
// error and the run stays failed, never completed_unscored.
func TestGameDayWithAMalformedReportStaysFailed(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = []byte(`{"runs": [`)
	w := &warnings{}
	gd, err := f.warnRunner(w).Run(f.ctx)
	if err == nil || gd.Status != StatusFailed || gd.EvalRunID != "" ||
		!strings.Contains(gd.Error, "ingest game-day report") {
		t.Fatalf("game day = %+v (%v), want failed on ingest", gd, err)
	}
	if lines := w.all(); len(lines) != 0 {
		t.Fatalf("warnings = %q, a failed run is not an unscored one", lines)
	}
	list, err := f.store.List(f.ctx, f.database, 10)
	if err != nil || len(list) != 1 || list[0].Status != StatusFailed {
		t.Fatalf("stored = %+v (%v)", list, err)
	}
}

// Local bench runs share the summary and the status (no database: the
// ledger is a fake).
func TestLocalBenchRunThatScoredNothingIsCompletedUnscored(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	w := &warnings{}
	b, err := NewLocalBench(LocalBenchConfig{Database: "orders", Provider: "dle",
		Now: func() time.Time { return at }, Warn: w.warn}, &fakeProvider{},
		&fakeFaults{report: runsReport(at, skippedRun("a", skipPrepared))},
		&fakeBenchLedger{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := b.Run(context.Background(), []string{"lock_blocking"})
	if err != nil || run.Status != StatusCompletedUnscored || run.EvalRunID != "eval-1" ||
		run.Summary == nil || run.Summary.Skipped != 1 || run.Summary.Scored != 0 {
		t.Fatalf("local run = %+v (%v)", run, err)
	}
	if last, _ := b.Last(); last.Status != StatusCompletedUnscored || last.Summary == nil {
		t.Fatalf("last = %+v", last)
	}
	if lines := w.all(); len(lines) != 1 || !strings.Contains(lines[0], skipPrepared) {
		t.Fatalf("warnings = %q", lines)
	}
}

func TestLocalBenchRunThatScoredIsCompleted(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	w := &warnings{}
	b, err := NewLocalBench(LocalBenchConfig{Database: "orders", Provider: "dle",
		Now: func() time.Time { return at }, Warn: w.warn}, &fakeProvider{},
		&fakeFaults{report: runsReport(at, scoredRun("a"))}, &fakeBenchLedger{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := b.Run(context.Background(), nil)
	if err != nil || run.Status != StatusCompleted || run.Summary == nil ||
		run.Summary.Scored != 1 || len(w.all()) != 0 {
		t.Fatalf("local run = %+v (%v), warnings %q", run, err, w.all())
	}
}

// A run that fails before any report has no summary (nil, not zero).
func TestLocalBenchRunWithoutAReportHasNoSummary(t *testing.T) {
	b := newLocalBench(t, &fakeProvider{createErr: fmt.Errorf("no snapshot")},
		&fakeFaults{}, &fakeBenchLedger{})
	run, err := b.Run(context.Background(), nil)
	if err == nil || run.Status != StatusFailed || run.Summary != nil {
		t.Fatalf("local run = %+v (%v), want failed with no summary", run, err)
	}
}
