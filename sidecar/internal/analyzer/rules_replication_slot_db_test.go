package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// A logical slot created on a real server and left idle raises nothing
// before analyzer.inactive_slot_min_minutes, then an advisory finding
// with no executable SQL (PG 14 uses the in-memory clock, PG 17+ the
// server's inactive_since).
func TestInactiveSlotRule_RealSlotWaitsForThreshold(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	var version int
	var walLevel string
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		current_setting('wal_level')`).Scan(&version, &walLevel); err != nil {
		t.Fatalf("read server version and wal_level: %v", err)
	}
	if walLevel != "logical" {
		t.Skipf("wal_level=%s: logical slots need wal_level=logical", walLevel)
	}
	name := fmt.Sprintf("rules09_idle_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx,
		`SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_drop_replication_slot($1)`, name)
	})
	snap := firstSnapshot(t, pool, version)
	start := time.Now()
	clk := &slotTestClock{now: start}
	ex := clk.extras()
	ident := "slot:" + name
	for _, after := range []time.Duration{0, 4 * time.Minute} {
		clk.now = start.Add(after)
		if f := findingFor(ruleInactiveSlots(snap, nil, phase2Config(), ex), ident); f != nil {
			t.Fatalf("idle %s: early finding %+v", after, f)
		}
	}
	clk.now = start.Add(6 * time.Minute)
	f := findingFor(ruleInactiveSlots(snap, nil, phase2Config(), ex), ident)
	if f == nil || f.Category != "inactive_slot" || f.RecommendedSQL != "" ||
		f.ActionRisk != "high_risk" || f.Detail["manual_sql"] == nil {
		t.Fatalf("after 6 min: %+v, want an advisory inactive_slot with manual_sql", f)
	}
}

func firstSnapshot(t *testing.T, pool *pgxpool.Pool, version int) *collector.Snapshot {
	t.Helper()
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	coll := collector.New(pool, cfg, version, noopLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coll.Run(ctx); close(done) }()
	deadline := time.Now().Add(60 * time.Second)
	for coll.LatestSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	snap := coll.LatestSnapshot()
	if snap == nil || snap.Replication == nil {
		t.Fatalf("collector gave no replication data: %+v", snap)
	}
	return snap
}

func findingFor(fs []Finding, ident string) *Finding {
	for i := range fs {
		if fs[i].ObjectIdentifier == ident {
			return &fs[i]
		}
	}
	return nil
}
