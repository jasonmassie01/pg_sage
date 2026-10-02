package collector

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// advisor.enabled now defaults to on, but the advisor only runs with a
// usable LLM. The runtime turns the advisor's per-cycle configuration
// snapshot (pg_settings and every table's reloptions) off when no
// advisor will consume it, so a deployment without an LLM issues no
// extra catalog queries.

func serverVersionNum(t *testing.T, c *Collector) int {
	t.Helper()
	var version int
	if err := c.pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	return version
}

func defaultsCollector(t *testing.T) *Collector {
	t.Helper()
	pool := testPool(t)
	cfg := config.DefaultConfig()
	cfg.HasWALColumns, cfg.HasPlanTimeColumns = true, true
	probe := New(pool, cfg, 0, noopLog)
	return New(pool, cfg, serverVersionNum(t, probe), noopLog)
}

func TestCollectorSkipsConfigSnapshotWithoutAdvisorConsumer(t *testing.T) {
	c := defaultsCollector(t)
	if !c.cfg.Advisor.Enabled || !c.CollectsConfigSnapshots() {
		t.Fatal("precondition: default advisor.enabled collects the snapshot")
	}
	c.WithoutConfigSnapshots()
	if c.CollectsConfigSnapshots() {
		t.Fatal("CollectsConfigSnapshots = true after WithoutConfigSnapshots")
	}
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.ConfigData != nil {
		t.Errorf("ConfigData collected with no advisor consumer: %d settings",
			len(snap.ConfigData.PGSettings))
	}
	if len(snap.Tables) == 0 && len(snap.Queries) == 0 && snap.System.MaxConnections == 0 {
		t.Error("the rest of the snapshot was skipped too")
	}
}

func TestCollectorKeepsConfigSnapshotForAdvisorConsumer(t *testing.T) {
	c := defaultsCollector(t)
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.ConfigData == nil || len(snap.ConfigData.PGSettings) == 0 {
		t.Fatalf("ConfigData = %+v, want pg_settings for the advisor", snap.ConfigData)
	}
}

// advisor.enabled=false still wins over a collector left at its default.
func TestCollectorAdvisorDisabledNeverSnapshotsConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Advisor.Enabled = false
	c := New(nil, cfg, 160000, noopLog)
	if c.CollectsConfigSnapshots() {
		t.Error("CollectsConfigSnapshots = true with advisor.enabled=false")
	}
}
