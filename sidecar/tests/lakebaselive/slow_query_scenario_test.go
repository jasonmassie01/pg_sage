package lakebaselive

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// TestLakebaseDefaultSlowQuery uses one constant-result, sleeping statement.
// It performs no workload table scan, CPU load, or statistics reset. A harmless
// constant-expression shape makes its queryID fresh without relying on aliases,
// which PostgreSQL ignores when calculating a queryID.
func TestLakebaseDefaultSlowQuery(t *testing.T) {
	p, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	nonce := uint64(time.Now().UnixNano())
	label := fmt.Sprintf("nerdslow_%x", nonce)
	var collision bool
	if err := p.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_statements
	 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND strpos(query,$1)>0)`, label).Scan(&collision); err != nil || collision {
		t.Fatalf("slow fixture identity already present=%v err=%v", collision, err)
	}
	cfg := config.DefaultConfig()
	if cfg.Analyzer.SlowQueryThresholdMs != 1000 {
		t.Fatalf("expected production default1000ms, got%d", cfg.Analyzer.SlowQueryThresholdMs)
	}
	cfg.Collector.IntervalSeconds = 2
	cfg.Analyzer.IntervalSeconds = 2
	cfg.Advisor.Enabled = false
	cfg.Safety.QueryTimeoutMs = 10000
	shape := "0"
	for bit := 0; bit < 64; bit++ {
		if nonce&(1<<bit) != 0 {
			shape += " + 0"
		} else {
			shape += " - 0"
		}
	}
	query := "SELECT pg_sleep(1.2) AS " + label + ", (" + shape + ") AS constant_result"
	var raw []byte
	if err := p.QueryRow(ctx, "EXPLAIN (VERBOSE,FORMAT JSON) "+query).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var explain []struct {
		ID json.Number `json:"Query Identifier"`
	}
	if err := json.Unmarshal(raw, &explain); err != nil || len(explain) != 1 {
		t.Fatalf("queryID preflight EXPLAIN: %v", err)
	}
	expectedID, err := strconv.ParseInt(explain[0].ID.String(), 10, 64)
	if err != nil {
		u, e := strconv.ParseUint(explain[0].ID.String(), 10, 64)
		if e != nil {
			t.Fatal(e)
		}
		expectedID = int64(u)
	}
	if err := p.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND queryid=$1)
	 OR EXISTS(SELECT FROM sage.query_store WHERE queryid=$1)`, expectedID).Scan(&collision); err != nil || collision || expectedID == 0 {
		t.Fatalf("queryID collision=%v id=%d err=%v", collision, expectedID, err)
	}
	var started time.Time
	if err := p.QueryRow(ctx, "SELECT now()").Scan(&started); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	exec(t, p, ctx, query)
	elapsed := time.Since(start)
	var id, calls int64
	var mean float64
	if err := p.QueryRow(ctx, `SELECT queryid,calls,mean_exec_time FROM pg_stat_statements
	 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
	 AND userid=(SELECT oid FROM pg_roles WHERE rolname=current_user)
	 AND strpos(query,$1)>0 AND query ~ '^SELECT pg_sleep'`, label).Scan(&id, &calls, &mean); err != nil {
		t.Fatal(err)
	}
	if id != expectedID || calls != 1 || mean <= 1000 {
		t.Fatalf("expected one measured default-threshold slow statement: queryid=%d calls=%d mean_ms=%.2f", id, calls, mean)
	}
	ident := fmt.Sprintf("queryid:%d", id)
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// No broad deletes or reset of cluster/other statements. Retain the
		// immutable test log and source snapshot; remove only this exact
		// produced finding and query_store sample from the isolated test DB.
		if _, err := p.Exec(clean, "DELETE FROM sage.findings WHERE category='slow_query' AND object_identifier=$1 AND strpos(detail->>'query',$2)>0", ident, label); err != nil {
			t.Errorf("exact slow fixture finding cleanup: %v", err)
		}
		if _, err := p.Exec(clean, "DELETE FROM sage.query_store WHERE queryid=$1 AND captured_at >= $2", id, started); err != nil {
			t.Errorf("exact slow fixture sample cleanup: %v", err)
		}
	})
	runCtx, cancel := context.WithCancel(ctx)
	logFn := func(level, format string, args ...any) {
		if level == "ERROR" && runCtx.Err() == nil {
			t.Logf(level+" "+format, args...)
		}
	}
	c := collector.New(p, cfg, 180000, logFn)
	a := analyzer.New(p, cfg, c, nil, nil, nil, nil, logFn)
	a.WithDatabaseName("pg_sage_test")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.Run(runCtx) }()
	go func() { defer wg.Done(); a.Run(runCtx) }()
	defer func() { cancel(); wg.Wait() }()
	var findingID int64
	var queryText string
	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) {
		err := p.QueryRow(ctx, `SELECT id,detail->>'query' FROM sage.findings
	 WHERE category='slow_query' AND status='open' AND object_identifier=$1
	 AND (detail->>'threshold_ms')::float8=1000 AND (detail->>'mean_exec_ms')::float8>1000`, ident).Scan(&findingID, &queryText)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if findingID == 0 || !strings.Contains(queryText, label) {
		t.Fatalf("default slow-query durable finding absent for exact fixture%d", id)
	}
	// One zero-sleep execution of the same normalized statement also proves
	// ordinary cumulative-mean recovery without any statistics reset.
	exec(t, p, ctx, strings.Replace(query, "pg_sleep(1.2)", "pg_sleep(0.0)", 1))
	deadline = time.Now().Add(60 * time.Second)
	var resolved bool
	for time.Now().Before(deadline) {
		if err := p.QueryRow(ctx, "SELECT status='resolved' FROM sage.findings WHERE id=$1", findingID).Scan(&resolved); err == nil && resolved {
			break
		}
		time.Sleep(time.Second)
	}
	if !resolved {
		t.Fatal("default slow query did not resolve after new zero-sleep execution")
	}
	t.Logf("default threshold 1000ms; queryid=%d finding_id=%d first_calls=1 first_mean_ms=%.2f elapsed_ms=%d; recovered after one zero-sleep call; no table scans or resets; exact fixture finding/sample cleanup, non-slow provider counters retained", id, findingID, mean, elapsed.Milliseconds())
}
