package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// footprint-01, integration (real Postgres): a collector interval promoted
// by self-config reaches the running collector. The engine writes the
// runtime config the collector holds; the collector moves its ticker
// between cycles, so sage.snapshots rows after the promotion are the
// promoted period apart, and the self-cost normalized to the configured
// interval is the cost of one real cycle.

// promoteCollectorInterval drives the engine with a short soak until the
// collector interval is promoted from 60 s to 120 s.
func promoteCollectorInterval(t *testing.T, r *selfConfigRunner, cfg *config.Config) {
	t.Helper()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	r.engine.Now = func() time.Time { return now }
	// 1150 ms of catalog and statements per cycle needs 115 s for a 1%
	// budget; the rule's 30 s step makes it 120 s.
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{CollectorCycleMs: selfconfig.Known(1150),
			Relations: selfconfig.Known(9000)}, nil
	}
	if _, err := r.pass(t.Context(), selfconfig.PhaseStartup); err != nil {
		t.Fatal(err)
	}
	var got selfconfig.Result
	for i := 0; i < 4; i++ {
		now = now.Add(15 * time.Minute)
		results, err := r.pass(t.Context(), selfconfig.PhaseLive)
		if err != nil {
			t.Fatal(err)
		}
		got = byKey(results)["collector.interval_seconds"]
	}
	if got.Status != selfconfig.StatusDerived || got.Value != 120 || !got.Applied {
		t.Fatalf("collector interval not promoted after a 1 h soak: %+v", got)
	}
	config.RLockForHotReload()
	secs := cfg.Collector.IntervalSeconds
	config.RUnlockForHotReload()
	if secs != 120 {
		t.Fatalf("runtime interval %d, want 120", secs)
	}
}

// snapshotTimes is every system snapshot written at or after since.
func snapshotTimes(t *testing.T, r *selfConfigRunner, since time.Time) []time.Time {
	t.Helper()
	rows, err := r.pool.Query(t.Context(), `SELECT DISTINCT collected_at
		FROM sage.snapshots WHERE category = 'system' AND collected_at >= $1
		ORDER BY collected_at`, since)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitForSnapshots(t *testing.T, r *selfConfigRunner, since time.Time,
	n int, within time.Duration) []time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := snapshotTimes(t, r, since)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Second)
	}
}

func startTestCollector(t *testing.T, r *selfConfigRunner, cfg *config.Config,
	logs *logCapture) *collector.Collector {
	t.Helper()
	var version int
	if err := r.pool.QueryRow(t.Context(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	c := collector.New(r.pool, cfg, version, func(level, msg string, args ...any) {
		logs.log(level, msg, args...)
	})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	return c
}

func TestSelfConfigPromotedIntervalReachesTheCollector(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the collector for five minutes of real time")
	}
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	cfg.SelfConfig.SoakHours = 1
	cfg.Safety.CPUCeilingPct = 100
	cfg.Advisor.Enabled = false
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	start := time.Now().Add(-time.Second)
	coll := startTestCollector(t, r, cfg, logs)
	if first := waitForSnapshots(t, r, start, 1, 30*time.Second); len(first) != 1 {
		t.Fatalf("no startup snapshot: %v\n%s", first, logs.text())
	}
	promoteCollectorInterval(t, r, cfg)
	if n := ledgerPromotions(t, r); n != 1 {
		t.Fatalf("ledger promotions %d, want 1", n)
	}
	// Startup, the tick still due at 60 s, then two promoted periods.
	got := waitForSnapshots(t, r, start, 4, 330*time.Second)
	if len(got) < 4 {
		t.Fatalf("snapshots %v, want 4 within 330 s\n%s", got, logs.text())
	}
	if !strings.Contains(logs.text(), "interval changed 60s -> 120s") {
		t.Fatalf("CHECK-01: no interval change logged:\n%s", logs.text())
	}
	for i := 1; i < 4; i++ {
		t.Logf("snapshot gap %d: %s", i, got[i].Sub(got[i-1]))
	}
	for i := 2; i < 4; i++ {
		gap := got[i].Sub(got[i-1])
		if math.Abs(gap.Seconds()-120) > 10 {
			t.Fatalf("CHECK-02: snapshots %d and %d are %s apart, want 120 s +/- 10 s: %v",
				i-1, i, gap, got)
		}
	}
	if coll.Interval() != 120*time.Second {
		t.Fatalf("collector runs at %s, want 2m0s", coll.Interval())
	}
	assertCostPerRealCycle(t, cfg, got[2], got[3])
}

func ledgerPromotions(t *testing.T, r *selfConfigRunner) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.config_derivation
		WHERE key = 'collector.interval_seconds' AND event = 'promoted'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Regression: the self-cost (pg_sage_self_db_time_ms_per_cycle) is
// normalized to the configured interval. With the collector running at
// that interval, 50 ms of pg_sage DB time between two real cycles is
// 50 ms per cycle, not 50 ms scaled by a period the process is not running.
func assertCostPerRealCycle(t *testing.T, cfg *config.Config, from, to time.Time) {
	t.Helper()
	key := selfcost.StatementKey{QueryID: 1}
	prev := selfcost.Reading{At: from, StatementsKnown: true,
		Statements: map[selfcost.StatementKey]selfcost.StatementCounters{key: {}}}
	cur := selfcost.Reading{At: to, StatementsKnown: true, DBTimeMs: 50,
		Statements: map[selfcost.StatementKey]selfcost.StatementCounters{
			key: {TimeMs: 50, Calls: 1}}}
	config.RLockForHotReload()
	cycle := cfg.Collector.Interval()
	config.RUnlockForHotReload()
	c := selfcost.Between(prev, cur, cycle)
	if !c.DBTimeKnown || math.Abs(c.DBTimeMsPerCycle-50) > 5 {
		t.Fatalf("DB time per cycle %.1f ms (known %v), want 50 ms +/- 10%%",
			c.DBTimeMsPerCycle, c.DBTimeKnown)
	}
}
