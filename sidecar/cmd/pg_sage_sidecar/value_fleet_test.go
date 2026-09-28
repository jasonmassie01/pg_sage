package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/value"
)

// T2: in meta-db mode the ledger lives in each monitored database. The
// API, metrics and MCP must read the target, never the meta database.
func TestValueMetaModeReadsTargetLedger(t *testing.T) {
	// Created first so its drop runs after the target runtime is drained.
	targetDSN := testdb.CreateDatabase(t, "meta_value_target")
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	preserveValueGlobals(t)
	ctx := context.Background()
	parsed, err := url.Parse(targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	input.DatabaseName = strings.TrimPrefix(parsed.Path, "/")
	rec, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil {
		t.Fatalf("register target: %v", err)
	}
	target := fleetMgr.GetInstance(input.Name)
	if target == nil || target.Pool == nil {
		t.Fatal("target runtime missing")
	}
	metaID := int64(rec.ID)
	seedFleetCredit(t, ctx, target.Pool, &metaID, "analyze_table", 15)
	seedFleetCredit(t, ctx, state.Pool, nil, "meta_sentinel", 999)
	pool = state.Pool

	metrics := scrapeMetrics(t)
	wantSeries := `pg_sage_toil_minutes_saved{database="managed-a",feature="analyze_table"} 15`
	if !strings.Contains(metrics, wantSeries) || strings.Contains(metrics, "meta_sentinel") {
		t.Fatalf("metrics did not read the target ledger only:\n%s", metrics)
	}
	mcpReport := mcpValueReport(t, &fleetMCPAccess{manager: fleetMgr, fallback: state.Pool})
	assertSingleDatabaseValue(t, "mcp", mcpReport, "managed-a", 0.25)

	router := wireRouter(WireParams{
		Cfg: cfg, Pool: state.Pool, FleetMgr: fleetMgr, MetaState: state,
	}).Handler
	w := adminAPIRequest(t, router, state.Pool, userID, http.MethodGet,
		"/api/v1/value", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/value = %d: %s", w.Code, w.Body.String())
	}
	var apiReport value.Report
	if err := json.Unmarshal(w.Body.Bytes(), &apiReport); err != nil {
		t.Fatal(err)
	}
	assertSingleDatabaseValue(t, "api", apiReport, "managed-a", 0.25)
}

// T3: YAML fleet mode has no global pool, yet every database's value is
// exported with its own label, and a down database is visible as up=0.
func TestValueMetricsFleetModeEmitsPerDatabase(t *testing.T) {
	preserveValueGlobals(t)
	ctx := context.Background()
	pools := fleetValuePools(t, "a", "b")
	seedFleetCredit(t, ctx, pools["a"], nil, "analyze_table", 15)
	seedFleetCredit(t, ctx, pools["b"], nil, "create_index", 30)
	pools["c"] = nil
	installValueFleet(pools)

	metrics := scrapeMetrics(t)
	for _, want := range []string{
		`pg_sage_toil_minutes_saved{database="a",feature="analyze_table"} 15`,
		`pg_sage_toil_minutes_saved{database="b",feature="create_index"} 30`,
		`pg_sage_value_metrics_up{database="a"} 1`,
		`pg_sage_value_metrics_up{database="b"} 1`,
		`pg_sage_value_metrics_up{database="c"} 0`,
		"# TYPE pg_sage_toil_minutes_saved gauge",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q in:\n%s", want, metrics)
		}
	}
	assertNoLeakedMetricLabels(t, metrics)
}

// T9: the MCP value tool aggregates every fleet database instead of
// failing for more than one instance.
func TestFleetMCPGetValueAggregates(t *testing.T) {
	preserveValueGlobals(t)
	ctx := context.Background()
	pools := fleetValuePools(t, "a", "b")
	seedFleetCredit(t, ctx, pools["a"], nil, "analyze_table", 15)
	seedFleetCredit(t, ctx, pools["b"], nil, "analyze_table", 30)
	installValueFleet(pools)

	report := mcpValueReport(t, &fleetMCPAccess{manager: fleetMgr})
	if report.DBAHoursSaved.AllTime != 0.75 || report.Partial {
		t.Fatalf("mcp aggregate = %+v", report)
	}
	want := []value.DatabaseHours{{Name: "b", Hours: 0.5}, {Name: "a", Hours: 0.25}}
	if !reflect.DeepEqual(report.ByDatabase, want) {
		t.Fatalf("mcp by_database = %+v", report.ByDatabase)
	}
	pools["c"] = nil
	installValueFleet(pools)
	partial := mcpValueReport(t, &fleetMCPAccess{manager: fleetMgr})
	if !partial.Partial || !reflect.DeepEqual(partial.Unavailable, []string{"c"}) ||
		partial.DBAHoursSaved.AllTime != 0.75 {
		t.Fatalf("mcp partial = %+v", partial)
	}
}

func TestFleetMCPGetValueEmptyFleetIsZero(t *testing.T) {
	preserveValueGlobals(t)
	installValueFleet(map[string]*pgxpool.Pool{})
	report := mcpValueReport(t, &fleetMCPAccess{manager: fleetMgr})
	if report.DBAHoursSaved.AllTime != 0 || report.Partial || len(report.ByDatabase) != 0 {
		t.Fatalf("empty fleet mcp value = %+v", report)
	}
}

func TestValueMetricsEmptyFleetEmitsHeadersOnly(t *testing.T) {
	var b strings.Builder
	writeValueMetrics(&b, context.Background(), nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_value_metrics_up gauge") ||
		strings.Contains(out, "pg_sage_value_metrics_up{") ||
		strings.Contains(out, "pg_sage_toil_minutes_saved{") {
		t.Fatalf("empty fleet metrics:\n%s", out)
	}
}

func assertSingleDatabaseValue(
	t *testing.T, surface string, report value.Report, name string, hours float64,
) {
	t.Helper()
	if report.DBAHoursSaved.AllTime != hours || report.Partial {
		t.Fatalf("%s value = %+v, want %v hours complete", surface, report, hours)
	}
	want := []value.DatabaseHours{{Name: name, Hours: hours}}
	if !reflect.DeepEqual(report.ByDatabase, want) {
		t.Fatalf("%s by_database = %+v, want %+v", surface, report.ByDatabase, want)
	}
}

func assertNoLeakedMetricLabels(t *testing.T, metrics string) {
	t.Helper()
	for _, bad := range []string{`database=""`, `database="all"`} {
		if strings.Contains(metrics, bad) {
			t.Fatalf("metrics leaked %s:\n%s", bad, metrics)
		}
	}
}

func mcpValueReport(t *testing.T, access *fleetMCPAccess) value.Report {
	t.Helper()
	result, err := access.GetValue(context.Background())
	if err != nil {
		t.Fatalf("mcp GetValue: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var report value.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	for _, row := range report.ByDatabase {
		if row.Name == "" || row.Name == "all" {
			t.Fatalf("mcp label leaked %q", row.Name)
		}
	}
	return report
}

func scrapeMetrics(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", w.Code)
	}
	return w.Body.String()
}

func preserveValueGlobals(t *testing.T) {
	t.Helper()
	oldCfg, oldPool, oldMgr := cfg, pool, fleetMgr
	t.Cleanup(func() { cfg, pool, fleetMgr = oldCfg, oldPool, oldMgr })
}

func installValueFleet(pools map[string]*pgxpool.Pool) {
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	pool = nil
	fleetMgr = fleet.NewManager(cfg)
	for name, p := range pools {
		fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Pool: p, Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: p != nil, LastSeen: time.Now()},
		})
	}
}

func fleetValuePools(t *testing.T, names ...string) map[string]*pgxpool.Pool {
	t.Helper()
	pools := make(map[string]*pgxpool.Pool, len(names))
	for _, name := range names {
		p, err := pgxpool.New(context.Background(),
			testdb.CreateDatabase(t, "cmd_value_"+name))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		t.Cleanup(p.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		err = schema.Bootstrap(ctx, p)
		cancel()
		if err != nil {
			t.Fatalf("bootstrap %s: %v", name, err)
		}
		pools[name] = p
	}
	return pools
}

func seedFleetCredit(
	t *testing.T, ctx context.Context, p *pgxpool.Pool, databaseID *int64,
	actionType string, minutes float64,
) {
	t.Helper()
	var id int64
	err := p.QueryRow(ctx, `INSERT INTO sage.action_log
		(database_id, action_type, sql_executed, outcome,
		 toil_minutes_saved, toil_model_version)
		VALUES ($1, $2, 'SELECT 1', 'success', $3, 1) RETURNING id`,
		databaseID, actionType, minutes).Scan(&id)
	if err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
}
