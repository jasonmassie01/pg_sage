package optimizer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (indexes-06): CREATE EXTENSION hypopg after pg_sage started
// is noticed at the next recheck, without a restart.

func flipDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.CreateDatabase(t, "hypopg_flip"))
	if err != nil {
		t.Fatalf("connect to the flip database: %v", err)
	}
	t.Cleanup(pool.Close)
	var available bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS
		(SELECT 1 FROM pg_available_extensions WHERE name='hypopg')`).Scan(&available); err != nil {
		t.Fatalf("read pg_available_extensions: %v", err)
	}
	if !available {
		t.Skip("HypoPG server extension unavailable; the availability flip is not verified")
	}
	return pool
}

func TestHypoPGAvailability_CreateExtensionFlipsWithoutRestart(t *testing.T) {
	pool := flipDatabase(t)
	clock := &testClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	h := NewHypoPG(pool, noopLog2)
	h.now = clock.Now
	if h.IsAvailable(t.Context()) {
		t.Fatal("HypoPG reported available before CREATE EXTENSION")
	}
	if _, err := pool.Exec(t.Context(), "CREATE EXTENSION hypopg"); err != nil {
		t.Fatalf("create extension hypopg: %v", err)
	}
	clock.advance(hypopgRecheck - time.Second)
	if h.IsAvailable(t.Context()) {
		t.Fatal("an absent verdict was re-probed inside the recheck window")
	}
	clock.advance(time.Second)
	if !h.IsAvailable(t.Context()) {
		t.Fatal("CREATE EXTENSION hypopg was not noticed at the recheck")
	}
	if _, err := pool.Exec(t.Context(), "DROP EXTENSION hypopg"); err != nil {
		t.Fatalf("drop extension hypopg: %v", err)
	}
	clock.advance(24 * time.Hour)
	if !h.IsAvailable(t.Context()) {
		t.Fatal("a found extension must stay cached (only false is re-probed)")
	}
}

// The probe is a bounded catalog read (statement and lock timeouts set in
// its read-only transaction), and a failed probe is not cached.
func TestHypoPGAvailability_ProbeIsBoundedAndErrorsUncached(t *testing.T) {
	base := flipDatabase(t)
	cfg := base.Config().Copy()
	recorder := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connect traced pool: %v", err)
	}
	defer pool.Close()
	logs := &logRecorder{}
	h := NewHypoPG(pool, logs.log)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if h.IsAvailable(cancelled) || h.available != nil {
		t.Fatal("a failed probe reported HypoPG available or was cached")
	}
	if len(logs.matching("HypoPG availability", "canceled")) != 1 {
		t.Fatalf("the failed probe was not logged with its cause: %q", logs.lines)
	}
	recorder.Reset()
	if h.IsAvailable(t.Context()) || h.available == nil {
		t.Fatal("the probe after a failure did not run and record HypoPG absent")
	}
	if len(recorder.Matching("set_config('statement_timeout'", "set_config('lock_timeout'")) !=
		1 || len(recorder.Matching("pg_extension", "hypopg")) != 1 {
		t.Fatalf("the probe was not one bounded catalog read: %+v",
			recorder.Matching(""))
	}
}

// A statement HypoPG cannot plan is named in UnplannedQueryIDs.
func TestHypoPGValidateNamesUnplannedStatements(t *testing.T) {
	pool := hypopgSessionPool(t)
	h := NewHypoPG(pool, noopLog2)
	queries := []QueryInfo{
		{QueryID: 11, Calls: 10, TotalTimeMs: 100,
			Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"},
		{QueryID: 22, Calls: 10, TotalTimeMs: 50,
			Text: "SELECT id FROM hypopg_session_test.items WHERE no_such_column=1"},
	}
	res, err := h.Validate(t.Context(), Recommendation{DDL: hypoCandidate}, queries)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.Measured != 1 || res.Failed != 1 ||
		!reflect.DeepEqual(res.UnplannedQueryIDs, []int64{22}) {
		t.Fatalf("result = %+v, want 1 measured and queryid 22 unplanned", res)
	}
	verdict, reason := whatIfVerdict(res, nil, 10)
	if verdict != WhatIfUnverified || !strings.HasSuffix(reason, "not planned: queryid 22") ||
		!strings.Contains(reason, "(call-weighted, 1 of 2)") {
		t.Fatalf("verdict %q reason %q", verdict, reason)
	}
}
