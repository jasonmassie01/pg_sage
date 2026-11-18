package api

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// drill-02a on a real server: the brief's options come from the
// database's confirmed facts and cost.rates, and the MCP brief stores a
// changed brief once. No concurrency test here: the store's is
// upgradecheck.TestBriefDB_ConcurrentStoreKeepsOneRow.

func briefLivePool(t *testing.T, label string, bootstrap bool) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if bootstrap {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}
	var name string
	var major int
	if err := pool.QueryRow(ctx, `SELECT current_database(),
		current_setting('server_version_num')::int / 10000`).Scan(&name, &major); err != nil {
		t.Fatalf("database: %v", err)
	}
	if major >= 18 {
		t.Skipf("PostgreSQL %d: no known major above it to plan an upgrade to", major)
	}
	return pool, name
}

func briefRateConfig(database string) *config.Config {
	compute, ext := 0.5, 0.1
	return &config.Config{Cost: config.CostConfig{Rates: []config.CostRate{{
		Database: database, Currency: "USD", AsOf: "2026-11-01", ComputePerHour: &compute,
		ExtendedSupportPerVCPUHour: &ext, VCPUs: 4}}}}
}

func TestUpgradeBriefMCP_LiveFactsRatesAndDeltaStore(t *testing.T) {
	pool, name := briefLivePool(t, "upgbrief", true)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot('brief_cdc',
		'test_decoding')`); err != nil {
		t.Fatalf("slot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_drop_replication_slot('brief_cdc')`)
	})
	if _, err := facts.NewStore(pool).Declare(ctx, facts.Proposal{
		Type: facts.TypeSlotConsumer, Kind: facts.KindSlot, Subject: "brief_*",
		Value: map[string]string{"consumer": "debezium"}}, "user:1", "cdc"); err != nil {
		t.Fatalf("declare: %v", err)
	}
	cfg := briefRateConfig(name)
	opts := upgradeBriefOptions(ctx, cfg, name, pool)
	if opts.SlotConsumersErr != nil || opts.SlotConsumers["brief_*"] != "debezium" ||
		opts.Rate == nil || opts.Rate.PerVCPUHour != 0.1 || opts.Rate.VCPUs != 4 ||
		opts.Rate.AsOf != "2026-11-01" {
		t.Fatalf("options = %+v (rate %+v)", opts, opts.Rate)
	}
	target := upgradecheck.Target{Major: 18, Method: upgradecheck.Logical, Provider: "rds"}
	first := mcpBrief(t, cfg, name, pool, target)
	if !first.Stored || first.BriefID == 0 || first.StoreError != "" ||
		len(first.SlotPlan.Items) != 1 || first.SlotPlan.Items[0].Consumer != "debezium" ||
		first.CostOfWaiting.Status != upgradecheck.CostPriced ||
		first.CostOfWaiting.PerMonth != 292 {
		t.Fatalf("first brief = %+v", first)
	}
	again := mcpBrief(t, cfg, name, pool, target)
	if again.Stored || again.BriefID != first.BriefID || !again.StoredAt.Equal(first.StoredAt) {
		t.Fatalf("unchanged brief = stored %v id %d, want the first row %d", again.Stored,
			again.BriefID, first.BriefID)
	}
}

func mcpBrief(t *testing.T, cfg *config.Config, name string, pool *pgxpool.Pool,
	target upgradecheck.Target) upgradeBriefView {
	t.Helper()
	got, err := UpgradeBriefMCP(context.Background(), cfg, name, pool, target,
		upgradecheck.Options{})
	if err != nil {
		t.Fatalf("UpgradeBriefMCP: %v", err)
	}
	view, ok := got.(upgradeBriefView)
	if !ok {
		t.Fatalf("UpgradeBriefMCP returned %T", got)
	}
	return view
}

func TestUpgradeBriefMCP_NoSageSchemaStillServesTheBrief(t *testing.T) {
	pool, name := briefLivePool(t, "upgbriefraw", false)
	opts := upgradeBriefOptions(context.Background(), nil, name, pool)
	if opts.SlotConsumersErr == nil || opts.Rate != nil {
		t.Fatalf("options = %+v, want the facts read error and no rate", opts)
	}
	view := mcpBrief(t, nil, name, pool, upgradecheck.Target{Major: 18,
		Provider: "self-managed"})
	if view.Stored || view.BriefID != 0 || !strings.Contains(view.StoreError, "upgrade_briefs") ||
		view.Database != name || len(view.Checklist) != 7 {
		t.Fatalf("view = %+v, want the brief with the store error", view)
	}
}
