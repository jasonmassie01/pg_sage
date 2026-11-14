package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: the epoch read and the capability state run
// on the single collector goroutine under Collector.mu.

// A database without pg_stat_statements (a fleet member that never ran
// CREATE EXTENSION) has no info view: the epoch is the postmaster start,
// as before security-01c-q, not an "epoch unknown" warning every cycle.
func TestStatementsEpoch_ExtensionAbsentIsPostmasterStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "collepochnopgss"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop extension: %v", err)
	}
	var start time.Time
	if err := pool.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&start); err != nil {
		t.Fatalf("postmaster start: %v", err)
	}
	var logs []string
	cfg := testConfig()
	cfg.StatementsSchema = ""
	c := New(pool, cfg, 170000, func(level, f string, args ...any) {
		logs = append(logs, level+" "+fmt.Sprintf(f, args...))
	})
	c.refreshCapabilities(ctx)
	if c.Capabilities().StatStatementsView {
		t.Fatal("premise: the probe found a pg_stat_statements view")
	}
	if epoch := c.collectStatementsEpoch(ctx); !epoch.Equal(start) {
		t.Fatalf("epoch = %v, want the postmaster start %v", epoch, start)
	}
	for _, l := range logs {
		if strings.Contains(l, "epoch unknown") {
			t.Fatalf("absent extension logged %q", l)
		}
	}
}

// A system-stats fallback invalidates the capabilities mid-cycle; the
// statements reads before the next refresh keep the probed schema rather
// than an empty one (every read failing as "not installed").
func TestInvalidateCapabilities_KeepsStatementsSchema(t *testing.T) {
	c := &Collector{cfg: &config.Config{StatementsSchema: "startup"}}
	c.caps = capState{probed: true, cached: true,
		key: capKey{version: 170000, viewOID: 7, extVersion: "1.11", schema: "ext"}}
	c.invalidateCapabilities()
	if got := c.statementsSchema(); got != "ext" {
		t.Fatalf("statementsSchema after invalidate = %q, want the probed %q", got, "ext")
	}
	if c.caps.cached || c.caps.key != (capKey{schema: "ext"}) {
		t.Fatalf("invalidate left %+v cached=%v, want only the schema kept",
			c.caps.key, c.caps.cached)
	}
	names, err := c.pgss()
	if err != nil || names.view != `"ext"."pg_stat_statements"` {
		t.Fatalf("pgss after invalidate = %+v, %v", names, err)
	}
}
