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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// TestLakebaseDefaultSlowQuery uses one constant-result, sleeping statement.
// It performs no workload table scan, CPU load, or statistics reset. The
// slow_query rule judges the interval mean with at least
// analyzer.slow_query_min_calls (default 5) calls in a collector interval
// (rules-05), so a few connections repeat the statement until the finding
// opens, then repeat its zero-sleep form until a measured fast interval
// resolves it. A harmless
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
	if cfg.Analyzer.SlowQueryMinCalls != 5 {
		t.Fatalf("expected production default call floor 5, got %d",
			cfg.Analyzer.SlowQueryMinCalls)
	}
	cfg.Collector.IntervalSeconds = 4
	cfg.Analyzer.IntervalSeconds = 4
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
	// Three sleeping connections finish about ten calls per 4 s interval.
	stopSlow := repeatStatement(runCtx, p, query, 3, 0)
	var findingID int64
	var queryText string
	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) {
		err := p.QueryRow(ctx, `SELECT id,detail->>'query' FROM sage.findings
	 WHERE category='slow_query' AND status='open' AND object_identifier=$1
	 AND (detail->>'threshold_ms')::float8=1000 AND (detail->>'interval_mean_ms')::float8>1000
	 AND (detail->>'interval_calls')::int8>=5`, ident).Scan(&findingID, &queryText)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err := stopSlow(); err != nil {
		t.Fatalf("slow fixture workload: %v", err)
	}
	if findingID == 0 || !strings.Contains(queryText, label) {
		t.Fatalf("default slow-query durable finding absent for exact fixture%d", id)
	}
	// Zero-sleep executions of the same normalized statement give a measured
	// fast interval, which resolves the finding without any statistics
	// reset; an idle interval would not (it is no verdict).
	stopFast := repeatStatement(runCtx, p,
		strings.Replace(query, "pg_sleep(1.2)", "pg_sleep(0.0)", 1), 2, 100*time.Millisecond)
	deadline = time.Now().Add(60 * time.Second)
	var resolved bool
	for time.Now().Before(deadline) {
		if err := p.QueryRow(ctx, "SELECT status='resolved' FROM sage.findings WHERE id=$1", findingID).Scan(&resolved); err == nil && resolved {
			break
		}
		time.Sleep(time.Second)
	}
	if err := stopFast(); err != nil {
		t.Fatalf("zero-sleep fixture workload: %v", err)
	}
	if !resolved {
		t.Fatal("default slow query did not resolve after a measured zero-sleep interval")
	}
	t.Logf("default threshold 1000ms, call floor 5; queryid=%d finding_id=%d first_calls=1 first_mean_ms=%.2f elapsed_ms=%d; recovered on a measured zero-sleep interval; no table scans or resets; exact fixture finding/sample cleanup, non-slow provider counters retained", id, findingID, mean, elapsed.Milliseconds())
}

// repeatStatement runs sql on workers connections, pausing pause after each
// call, until ctx ends or the returned stop is called; stop waits for the
// workers and returns the first error that was not the stop itself.
func repeatStatement(ctx context.Context, p *pgxpool.Pool, sql string, workers int,
	pause time.Duration) func() error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if _, err := p.Exec(ctx, sql); err != nil {
					if ctx.Err() == nil {
						mu.Lock()
						if first == nil {
							first = fmt.Errorf("%s: %w", sql, err)
						}
						mu.Unlock()
					}
					return
				}
				select {
				case <-ctx.Done():
				case <-time.After(pause):
				}
			}
		}()
	}
	return func() error {
		cancel()
		wg.Wait()
		return first
	}
}
