package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// startedCollector returns a collector whose ticker period is set to secs,
// as Run sets it, without collecting: Run returns at once on a cancelled
// context, so no pool is touched.
func startedCollector(t *testing.T, secs int) *collector.Collector {
	t.Helper()
	cfg := &config.Config{Collector: config.CollectorConfig{IntervalSeconds: secs}}
	c := collector.New(nil, cfg, 170000, func(string, string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)
	if got := c.Interval(); got != time.Duration(secs)*time.Second {
		t.Fatalf("collector period %v, want %ds", got, secs)
	}
	return c
}

func registerWithCollector(
	mgr *DatabaseManager, name string, cfg config.DatabaseConfig,
	c *collector.Collector, lastRunAgo time.Duration,
) {
	s := freshStatus()
	s.CollectorLastRun = time.Now().Add(-lastRunAgo)
	cfg.Name = name
	mgr.RegisterInstance(&DatabaseInstance{
		Name: name, Config: cfg, Status: &s, Collector: c,
	})
}

// Staleness is judged against the period the collector actually runs at.
// The runtime config is a per-database clone that self-config promotions
// and hot reloads change, and databases[].collector_interval_seconds does
// not reach the collector, so the manager's config can disagree with it.
func TestFleetStatus_StalenessUsesRunningCollectorPeriod(t *testing.T) {
	mgr := healthManager() // global collector.interval_seconds = 60
	// Runs every 600 s: a sample 10 min old is one interval, not stale.
	registerWithCollector(mgr, "slowed", config.DatabaseConfig{},
		startedCollector(t, 600), 10*time.Minute)
	// Runs every 10 s: a sample 45 s old is 4.5 intervals, stale.
	registerWithCollector(mgr, "quick", config.DatabaseConfig{},
		startedCollector(t, 10), 45*time.Second)
	// The unwired override says 600 s, but the collector runs every 60 s.
	registerWithCollector(mgr, "override",
		config.DatabaseConfig{CollectorIntervalSeconds: 600},
		startedCollector(t, 60), 5*time.Minute)

	o := mgr.FleetStatus()
	if got := rowByName(t, o, "slowed").Status; got.HealthScore != 100 ||
		len(got.HealthReasons) != 0 {
		t.Fatalf("slowed: score %d reasons %q, want 100 and none",
			got.HealthScore, got.HealthReasons)
	}
	for _, name := range []string{"quick", "override"} {
		got := rowByName(t, o, name).Status
		if got.HealthScore != 25 || len(got.HealthReasons) != 1 {
			t.Fatalf("%s: score %d reasons %q, want 25 and one stale reason",
				name, got.HealthScore, got.HealthReasons)
		}
	}
}

// A collector that has not started its ticker (period 0) falls back to the
// configured interval.
func TestFleetStatus_UnstartedCollectorFallsBackToConfig(t *testing.T) {
	mgr := healthManager()
	cfg := &config.Config{Collector: config.CollectorConfig{IntervalSeconds: 600}}
	idle := collector.New(nil, cfg, 170000, func(string, string, ...any) {})
	registerWithCollector(mgr, "idle", config.DatabaseConfig{}, idle,
		5*time.Minute)
	got := rowByName(t, mgr.FleetStatus(), "idle").Status
	if got.HealthScore != 25 {
		t.Fatalf("idle: score %d reasons %q, want 25 (5 min against 60 s)",
			got.HealthScore, got.HealthReasons)
	}
}
