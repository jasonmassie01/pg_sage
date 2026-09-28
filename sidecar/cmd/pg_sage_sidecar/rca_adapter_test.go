package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func rcaAdapterTestConfig() *config.Config {
	return &config.Config{RCA: config.RCAConfig{
		Enabled: true, DedupWindowMinutes: 30, EscalationCycles: 5,
		ResolutionCycles: 2, ConnectionSaturationPct: 80,
	}}
}

func saturatedSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends: 90, MaxConnections: 100, CacheHitRatio: 0.999,
		},
	}
}

// TestRCAAdapterStampsIdentityAndPersists proves the adapter wires the
// database identity (R05) and the durable store (R04) into the engine.
func TestRCAAdapterStampsIdentityAndPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const alias = "adapter_alias_db"
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", alias)
	}
	clean()
	t.Cleanup(clean)

	cfg := rcaAdapterTestConfig()
	eng := rca.NewEngine(&cfg.RCA, func(string, string, ...any) {})
	a := newRCAAdapter(ctx, eng, pool, alias,
		func(string, string, ...any) {})
	a.Analyze(saturatedSnapshot(), nil, cfg, nil)
	if err := a.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.incidents
		WHERE database_name = $1 AND resolved_at IS NULL`,
		alias).Scan(&n); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if n != 1 {
		t.Fatalf("open incidents for %s = %d, want 1", alias, n)
	}
}

// TestRCAAdapterSkipsCycleWhenStateCannotLoad proves the engine does not
// run on an empty in-memory state when durable state is unreachable.
func TestRCAAdapterSkipsCycleWhenStateCannotLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx,
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := rcaAdapterTestConfig()
	eng := rca.NewEngine(&cfg.RCA, func(string, string, ...any) {})
	var warned bool
	a := newRCAAdapter(ctx, eng, pool, "unreachable",
		func(level, _ string, _ ...any) {
			if level == "WARN" {
				warned = true
			}
		})
	a.Analyze(saturatedSnapshot(), nil, cfg, nil)
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Fatalf("active incidents = %d, want 0 when hydrate failed", n)
	}
	if !warned {
		t.Error("skipped cycle was not logged at WARN")
	}
}

// TestRCAAdapterFastPathLifecycle: the first analyzer cycle starts the
// lock-chain fast path once; interval 0 leaves it off.
func TestRCAAdapterFastPathLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx,
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(pool.Close)
	noLog := func(string, string, ...any) {}

	on := rcaAdapterTestConfig()
	on.RCA.LockChainIntervalSeconds = 60
	a := newRCAAdapter(ctx, rca.NewEngine(&on.RCA, noLog), pool, "db", noLog)
	a.Analyze(saturatedSnapshot(), nil, on, nil)
	first := a.fastPath
	if first == nil || first.Interval() != 60*time.Second {
		t.Fatalf("fast path = %v, want started at 60s", first)
	}
	a.Analyze(saturatedSnapshot(), nil, on, nil)
	if a.fastPath != first {
		t.Fatal("second analyzer cycle started another fast path")
	}

	off := rcaAdapterTestConfig() // lock_chain_interval_seconds: 0
	b := newRCAAdapter(ctx, rca.NewEngine(&off.RCA, noLog), pool, "db", noLog)
	b.Analyze(saturatedSnapshot(), nil, off, nil)
	if b.fastPath != nil {
		t.Fatal("interval 0 must leave the fast path disabled")
	}
}
