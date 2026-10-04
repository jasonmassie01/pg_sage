package collector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Five-minute time to value: the first snapshot is taken at startup, not
// one full interval later (60 s by default).
func TestRunCollectsImmediately(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	cfg.Advisor.Enabled = false
	c := New(pool, cfg, version, func(string, string, ...any) {})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	defer func() { stop(); <-done }()
	start := time.Now()
	for c.LatestSnapshot() == nil && time.Since(start) < 30*time.Second {
		time.Sleep(20 * time.Millisecond)
	}
	if c.LatestSnapshot() == nil {
		t.Fatal("no snapshot within 30 s of start with a one-hour interval")
	}
	if c.PreviousSnapshot() != nil {
		t.Fatal("one startup cycle produced two snapshots")
	}
}

func TestRunStopsDuringTheFirstCycleWait(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 3600
	c := New(nil, cfg, 170000, func(string, string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for a cancelled context")
	}
	if c.LatestSnapshot() != nil {
		t.Fatal("a cancelled collector produced a snapshot")
	}
}
