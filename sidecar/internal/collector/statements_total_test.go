package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// evid-01a: the queries step also reads the database's statement totals
// (all entries but pg_sage's own, whatever the admission keeps) on the
// same statement, and collect copies them and the statements epoch into
// the system category, which is stored in full every cycle.
// No concurrent access tests: the totals ride on one cycle's snapshot,
// which one goroutine builds.

func TestStatementTotals_ReadOnTheCountersStatement(t *testing.T) {
	c, _ := textCollector(2)
	db := newFakeStatements(appTexts(1, 2, 3))
	db.candidates(1, 2, 3)
	var totals statementTotals
	qs, err := c.queriesVia(context.Background(), db, &totals)
	if err != nil {
		t.Fatalf("queries: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("admitted %d queries, want the cap of 2", len(qs))
	}
	want := statementTotals{execMs: counterTotalsExecMs, calls: counterTotalsCalls,
		entries: counterTotalsEntries, known: true}
	if totals != want {
		t.Fatalf("totals = %+v, want %+v (independent of the cap)", totals, want)
	}
}

func TestStatementTotals_NoCandidatesLeavesThemUnknown(t *testing.T) {
	c, _ := textCollector(2)
	db := newFakeStatements(nil)
	totals := statementTotals{execMs: 9, known: true}
	if _, err := c.queriesVia(context.Background(), db, &totals); err != nil {
		t.Fatalf("queries: %v", err)
	}
	if totals != (statementTotals{}) {
		t.Fatalf("no rows: totals = %+v, want unknown (zero)", totals)
	}
}

func TestStatementTotals_FailedReadLeavesThemUnknown(t *testing.T) {
	c, _ := textCollector(2)
	db := newFakeStatements(appTexts(1))
	db.candidates(1)
	db.countErr = errors.New("connection reset")
	totals := statementTotals{execMs: 9, known: true}
	if _, err := c.queriesVia(context.Background(), db, &totals); err == nil {
		t.Fatal("a failed counters read must fail the step")
	}
	if totals != (statementTotals{}) {
		t.Fatalf("failed read: totals = %+v, want unknown (zero)", totals)
	}
}

func TestApplyStatementTotals_CopiesEpochAndKnownTotals(t *testing.T) {
	epoch := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	snap := &Snapshot{StatsEpoch: epoch, stmtTotals: statementTotals{execMs: 12.5,
		calls: 40, entries: 3, known: true}}
	applyStatementTotals(snap)
	s := snap.System
	if !s.StatementsEpoch.Equal(epoch) || s.StatementsTotalExecMs != 12.5 ||
		s.StatementsCalls != 40 || s.StatementsEntries != 3 {
		t.Fatalf("system = epoch %v total %v calls %d entries %d", s.StatementsEpoch,
			s.StatementsTotalExecMs, s.StatementsCalls, s.StatementsEntries)
	}
	unknown := &Snapshot{StatsEpoch: epoch}
	unknown.System.StatementsTotalExecMs = 7
	applyStatementTotals(unknown)
	if unknown.System.StatementsTotalExecMs != 0 || unknown.System.StatementsEntries != 0 {
		t.Fatalf("unknown totals must stay zero, got %+v", unknown.System)
	}
	if !unknown.System.StatementsEpoch.Equal(epoch) {
		t.Fatal("the epoch is copied even when the queries step failed")
	}
}

func TestSystemStatsJSON_StatementFieldsOmittedWhenUnknown(t *testing.T) {
	plain, err := json.Marshal(SystemStats{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"statements_epoch", "statements_total_exec_ms",
		"statements_calls", "statements_entries"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("unknown %s marshals: %s", key, plain)
		}
	}
	epoch := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	in := SystemStats{StatementsEpoch: epoch, StatementsTotalExecMs: 1.25,
		StatementsCalls: 3, StatementsEntries: 2}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"statements_epoch":"2026-11-01T09:00:00Z"`) {
		t.Fatalf("epoch key missing: %s", raw)
	}
	var out SystemStats
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !out.StatementsEpoch.Equal(epoch) || out.StatementsTotalExecMs != 1.25 ||
		out.StatementsCalls != 3 || out.StatementsEntries != 2 {
		t.Fatalf("round trip = %+v", out)
	}
}

func TestQueryStatsFrom_TotalsLeaveOutOwnStatements(t *testing.T) {
	for _, want := range []string{"AS stmt_exec_ms", "AS stmt_calls", "AS stmt_entries",
		"FROM agg WHERE queryid <> ALL($2::int8[])"} {
		if !strings.Contains(queryStatsFrom, want) {
			t.Errorf("queryStatsFrom lacks %q", want)
		}
	}
	// The totals ride on the counters read: no second statement.
	if strings.Count(queryStatsFrom, "pg_stat_statements") > 0 {
		t.Error("queryStatsFrom names the view itself; it must read the qualified function")
	}
}
