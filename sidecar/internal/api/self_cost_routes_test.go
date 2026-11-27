package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// footprint-08: GET /api/v1/self-cost answers "what does pg_sage cost me,
// and which part?" per database, from each analyzer's self-cost meter.

func footprintConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Analyzer.SelfCostBudgetMs = 3000
	cfg.SelfBudget.DBTimeMsPerHour = 0
	cfg.SelfBudget.BlocksPerHour = 18000000
	cfg.SelfBudget.StorageMB = 10240
	cfg.SelfBudget.CPUMsPerCycle = 600
	return cfg
}

// footprintMgr registers one instance per name, each with an analyzer
// over its own meter (no database: the test feeds the meter).
func footprintMgr(cfg *config.Config, names ...string) (*fleet.DatabaseManager,
	map[string]*selfcost.Meter) {
	mgr := fleet.NewManager(cfg)
	meters := map[string]*selfcost.Meter{}
	for _, name := range names {
		m := selfcost.NewMeter()
		a := analyzer.New(nil, cfg, nil, nil, nil, nil, nil, func(string, string, ...any) {})
		a.WithSelfCostMeter(m)
		meters[name] = m
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Analyzer: a,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: true}})
	}
	return mgr, meters
}

var footprintT0 = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func footprintReading(db string, at time.Time, collectorMs, sreMs float64, calls, blocks,
	rowsRead, rowsWritten, schema int64) selfcost.Reading {
	return selfcost.Reading{At: at, Database: db, StatementsKnown: true,
		RowsRead: rowsRead, RowsWritten: rowsWritten, SchemaBytes: schema,
		Statements: map[selfcost.StatementKey]selfcost.StatementCounters{
			{UserID: 10, QueryID: 1, TopLevel: true}: {TimeMs: collectorMs, Calls: calls,
				Blocks: blocks, Text: "SELECT /* pg_sage collector */ count(*) FROM pg_class"},
			{UserID: 10, QueryID: 2, TopLevel: true}: {TimeMs: sreMs, Calls: calls / 2,
				Blocks: blocks / 5, Text: "SELECT /* pg_sage sre:probe v1 */ 1"},
		}}
}

// observeTwoCycles is two analyzer cycles 10 minutes apart on a 60 s
// collector: collector +300 ms, sre:probe +100 ms, 15 calls, 60 blocks,
// 600 rows read and 60 written in the window.
func observeTwoCycles(m *selfcost.Meter, db string) {
	m.Observe(footprintReading(db, footprintT0, 100, 500, 10, 50, 1000, 100, 4096), time.Minute)
	m.Observe(footprintReading(db, footprintT0.Add(10*time.Minute), 400, 600, 20, 100, 1600,
		160, 8192), time.Minute)
}

func getSelfCost(t *testing.T, mgr *fleet.DatabaseManager, path string,
	user *auth.User) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	registerSelfCostRoutes(mux, mgr)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var body map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w.Code, body
}

func onlyDatabase(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	dbs, _ := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases = %v, want one", body["databases"])
	}
	db := dbs[0].(map[string]any)
	if db["database"] != name {
		t.Fatalf("database = %v, want %s", db["database"], name)
	}
	return db
}

var footprintFigures = []string{"window_seconds", "cycle_seconds", "db_time_ms_per_hour",
	"calls_per_hour", "blocks_per_hour", "rows_read_per_hour", "rows_written_per_hour",
	"schema_bytes", "unattributed_share"}

func TestSelfCostRoute_BeforeFirstWindowEverythingNull(t *testing.T) {
	mgr, _ := footprintMgr(footprintConfig(), "orders")
	code, body := getSelfCost(t, mgr, "/api/v1/self-cost?database=orders", testViewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	db := onlyDatabase(t, body, "orders")
	for _, k := range footprintFigures {
		v, present := db[k]
		if !present || v != nil {
			t.Errorf("%s = %#v (present %v), want null", k, v, present)
		}
	}
	known := db["known"].(map[string]any)
	for _, k := range []string{"window", "statements", "storage"} {
		if known[k] != false {
			t.Errorf("known.%s = %v, want false", k, known[k])
		}
	}
	for _, k := range []string{"by_component", "top_statements"} {
		if rows, ok := db[k].([]any); !ok || len(rows) != 0 {
			t.Errorf("%s = %#v, want an empty list", k, db[k])
		}
	}
	process := body["process"].(map[string]any)
	if process["cpu_ms_per_cycle"] != nil || process["known"] != false {
		t.Errorf("process = %v, want CPU unknown", process)
	}
	if body["meaning"] == "" || body["meaning"] == nil {
		t.Error("meaning missing")
	}
}

// After one cycle the size and cycle are known but no rate is: one
// reading is not a window.
func TestSelfCostRoute_AfterOneCycleOnlySizeKnown(t *testing.T) {
	mgr, meters := footprintMgr(footprintConfig(), "orders")
	meters["orders"].Observe(footprintReading("orders", footprintT0, 1, 1, 1, 1, 1, 1, 4096),
		time.Minute)
	_, body := getSelfCost(t, mgr, "/api/v1/self-cost?database=orders", testViewerUser())
	db := onlyDatabase(t, body, "orders")
	if db["schema_bytes"] != 4096.0 || db["cycle_seconds"] != 60.0 {
		t.Fatalf("schema_bytes %v cycle_seconds %v, want 4096 and 60", db["schema_bytes"],
			db["cycle_seconds"])
	}
	for _, k := range []string{"db_time_ms_per_hour", "rows_read_per_hour", "window_seconds"} {
		if db[k] != nil {
			t.Errorf("%s = %v after one reading, want null", k, db[k])
		}
	}
	if known := db["known"].(map[string]any); known["storage"] != true ||
		known["window"] != false {
		t.Errorf("known = %v, want storage only", known)
	}
}

func TestSelfCostRoute_AfterTwoCyclesReportsKnownFigures(t *testing.T) {
	mgr, meters := footprintMgr(footprintConfig(), "orders")
	observeTwoCycles(meters["orders"], "orders")
	code, body := getSelfCost(t, mgr, "/api/v1/self-cost?database=orders", testViewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	db := onlyDatabase(t, body, "orders")
	for k, want := range map[string]float64{
		"window_seconds": 600, "cycle_seconds": 60, "db_time_ms_per_hour": 2400,
		"calls_per_hour": 90, "blocks_per_hour": 360, "rows_read_per_hour": 3600,
		"rows_written_per_hour": 360, "schema_bytes": 8192, "unattributed_share": 0,
		"db_time_budget_ms_per_hour": 180000, "blocks_budget_per_hour": 18000000,
		"storage_budget_bytes": float64(int64(10240) << 20),
	} {
		if got, ok := db[k].(float64); !ok || !nearly(got, want) {
			t.Errorf("%s = %#v, want %v", k, db[k], want)
		}
	}
	if db["db_time_budget_source"] != "analyzer.self_cost_budget_ms" {
		t.Errorf("db_time_budget_source = %v", db["db_time_budget_source"])
	}
	assertComponents(t, db)
	assertTopStatements(t, db)
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

func assertComponents(t *testing.T, db map[string]any) {
	t.Helper()
	rows, _ := db["by_component"].([]any)
	if len(rows) != 2 {
		t.Fatalf("by_component = %v, want collector and sre:probe", db["by_component"])
	}
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if first["component"] != "collector" || !nearly(first["db_time_ms_per_hour"].(float64), 1800) ||
		!nearly(first["share"].(float64), 0.75) || !nearly(first["calls_per_hour"].(float64), 60) {
		t.Errorf("first component = %v, want collector 1800 ms/h, share 0.75", first)
	}
	if second["component"] != "sre:probe" ||
		!nearly(second["db_time_ms_per_hour"].(float64), 600) ||
		!nearly(second["share"].(float64), 0.25) {
		t.Errorf("second component = %v, want sre:probe 600 ms/h, share 0.25", second)
	}
}

func assertTopStatements(t *testing.T, db map[string]any) {
	t.Helper()
	rows, _ := db["top_statements"].([]any)
	if len(rows) != 2 {
		t.Fatalf("top_statements = %v", db["top_statements"])
	}
	top := rows[0].(map[string]any)
	if top["component"] != "collector" || top["text"] != "SELECT count(*) FROM pg_class" ||
		!nearly(top["db_time_ms"].(float64), 300) || top["calls"] != 10.0 {
		t.Errorf("top statement = %v", top)
	}
}

// An explicit self_budget.db_time_ms_per_hour applies as-is; a disabled
// budget is null, not 0.
func TestSelfCostRoute_BudgetSources(t *testing.T) {
	cfg := footprintConfig()
	cfg.SelfBudget.DBTimeMsPerHour = 5000
	cfg.SelfBudget.BlocksPerHour = 0
	cfg.SelfBudget.StorageMB = 0
	mgr, meters := footprintMgr(cfg, "orders")
	observeTwoCycles(meters["orders"], "orders")
	_, body := getSelfCost(t, mgr, "/api/v1/self-cost", testViewerUser())
	db := onlyDatabase(t, body, "orders")
	if db["db_time_budget_ms_per_hour"] != 5000.0 ||
		db["db_time_budget_source"] != "self_budget.db_time_ms_per_hour" {
		t.Errorf("db time budget = %v from %v, want 5000 explicit",
			db["db_time_budget_ms_per_hour"], db["db_time_budget_source"])
	}
	for _, k := range []string{"blocks_budget_per_hour", "storage_budget_bytes"} {
		if v, present := db[k]; !present || v != nil {
			t.Errorf("%s = %#v, want null (disabled)", k, v)
		}
	}
	cfg.SelfBudget.DBTimeMsPerHour, cfg.Analyzer.SelfCostBudgetMs = 0, 0
	_, body = getSelfCost(t, mgr, "/api/v1/self-cost", testViewerUser())
	db = onlyDatabase(t, body, "orders")
	if db["db_time_budget_ms_per_hour"] != nil || db["db_time_budget_source"] != "none" {
		t.Errorf("no budget = %v from %v, want null from none",
			db["db_time_budget_ms_per_hour"], db["db_time_budget_source"])
	}
}

// Auth: every signed-in role reads it, no session is 401. An unknown
// database is 404 and a malformed name is 400.
func TestSelfCostRoute_AuthAndDatabase(t *testing.T) {
	mgr, _ := footprintMgr(footprintConfig(), "orders")
	cases := map[string]struct {
		path string
		user *auth.User
		want int
	}{
		"viewer":           {"/api/v1/self-cost?database=orders", testViewerUser(), 200},
		"operator":         {"/api/v1/self-cost?database=orders", testOperatorUser(), 200},
		"admin":            {"/api/v1/self-cost", testAdminUser(), 200},
		"no session":       {"/api/v1/self-cost?database=orders", nil, 401},
		"unknown database": {"/api/v1/self-cost?database=nope", testViewerUser(), 404},
		"malformed name":   {"/api/v1/self-cost?database=bad%2Aname", testViewerUser(), 400},
	}
	for name, tc := range cases {
		if code, _ := getSelfCost(t, mgr, tc.path, tc.user); code != tc.want {
			t.Errorf("%s: status %d, want %d", name, code, tc.want)
		}
	}
}

// Fleet: one row per database, sorted; the process CPU appears once.
func TestSelfCostRoute_FleetRowsPerDatabase(t *testing.T) {
	mgr, meters := footprintMgr(footprintConfig(), "zeta", "alpha")
	observeTwoCycles(meters["alpha"], "alpha")
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "stopped", Stopped: true,
		Status: &fleet.InstanceStatus{}})
	for _, path := range []string{"/api/v1/self-cost", "/api/v1/self-cost?database=all"} {
		_, body := getSelfCost(t, mgr, path, testViewerUser())
		dbs, _ := body["databases"].([]any)
		if len(dbs) != 3 {
			t.Fatalf("%s: databases = %v, want 3", path, dbs)
		}
		names := []string{}
		for _, d := range dbs {
			names = append(names, d.(map[string]any)["database"].(string))
		}
		if names[0] != "alpha" || names[1] != "stopped" || names[2] != "zeta" {
			t.Fatalf("%s: order = %v", path, names)
		}
		if dbs[0].(map[string]any)["db_time_ms_per_hour"] != 2400.0 {
			t.Errorf("alpha = %v", dbs[0])
		}
		for _, i := range []int{1, 2} {
			if dbs[i].(map[string]any)["db_time_ms_per_hour"] != nil {
				t.Errorf("%s has a rate before its window: %v", names[i], dbs[i])
			}
		}
		if _, ok := body["process"].(map[string]any); !ok {
			t.Fatalf("%s: process missing: %v", path, body)
		}
	}
}

// Concurrent access: requests while the analyzers observe. Run with
// -race where cgo is available; here it also checks every answer is whole.
func TestSelfCostRoute_ConcurrentWithCycles(t *testing.T) {
	mgr, meters := footprintMgr(footprintConfig(), "orders")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			at := footprintT0.Add(time.Duration(i) * time.Minute)
			meters["orders"].Observe(footprintReading("orders", at, float64(100+i*10), 500,
				int64(10+i), 50, int64(1000+i), 100, 4096), time.Minute)
		}
	}()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				code, body := getSelfCost(t, mgr, "/api/v1/self-cost?database=orders",
					testViewerUser())
				if code != http.StatusOK {
					t.Errorf("status %d", code)
					return
				}
				dbs, _ := body["databases"].([]any)
				if len(dbs) != 1 || dbs[0].(map[string]any)["known"] == nil {
					t.Errorf("answer not whole: %v", body)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The process CPU comes from the process watch (footprint-07), once for
// every database, with the busiest loops across the process; a disabled
// budget is null and an unknown reading has no figure.
func TestSelfCostProcessView_FromTheProcessWatch(t *testing.T) {
	cfg := footprintConfig()
	r := selfbudget.CPUReading{Known: true, MsPerCycle: 42, BudgetMs: 600,
		Loops: []selfbudget.LoopCost{{Name: "collector:b", BusyMs: 900, Runs: 3}}}
	v := processView(r, cfg)
	if !v.Known || v.CPUMs == nil || *v.CPUMs != 42 || v.CPUBudget == nil ||
		*v.CPUBudget != 600 {
		t.Fatalf("process = %+v, want 42 ms against 600", v)
	}
	if len(v.TopLoops) != 1 || v.TopLoops[0] != (footprintLoop{Loop: "collector:b",
		BusyMs: 900, Runs: 3}) {
		t.Fatalf("loops = %+v", v.TopLoops)
	}
	cfg.SelfBudget.CPUMsPerCycle = 0
	v = processView(selfbudget.CPUReading{}, cfg)
	if v.Known || v.CPUMs != nil || v.CPUBudget != nil || v.TopLoops == nil ||
		len(v.TopLoops) != 0 {
		t.Fatalf("unmeasured, no budget = %+v", v)
	}
}

// The registered process watch is what the handler reads.
func TestSelfCostProcessView_ReadsTheRegisteredWatch(t *testing.T) {
	prev := selfbudget.ProcessWatch()
	t.Cleanup(func() { selfbudget.SetProcessWatch(prev) })
	clock, cpu := time.Now(), time.Minute
	w := selfbudget.NewProcessCPUWatch(selfbudget.WatchOptions{
		Read:   func() (time.Duration, bool) { return cpu, true },
		Now:    func() time.Time { return clock },
		Budget: func() float64 { return 600 },
		Cycle:  func() time.Duration { return time.Minute }})
	_ = w.Tick(context.Background())
	clock, cpu = clock.Add(time.Minute), cpu+700*time.Millisecond
	_ = w.Tick(context.Background())
	selfbudget.SetProcessWatch(w)
	if got := processView(selfbudget.ProcessWatch().Reading(), footprintConfig()); !got.Known ||
		got.CPUMs == nil || *got.CPUMs != 700 {
		t.Fatalf("process = %+v, want the watch's 700 ms", got)
	}
}

// Boundary: a known window whose components used no time has shares of
// 0, not NaN, and an unattributed share of 0.
func TestFootprintComponents_ZeroTotal(t *testing.T) {
	rows, unattributed := footprintComponents([]selfcost.ComponentCost{
		{Component: "collector", CallsPerHour: 6}}, true)
	if len(rows) != 1 || rows[0].Share != 0 || unattributed == nil || *unattributed != 0 {
		t.Fatalf("rows %+v unattributed %v", rows, unattributed)
	}
	if rows, u := footprintComponents(nil, false); len(rows) != 0 || u != nil {
		t.Fatalf("unknown = %+v, %v", rows, u)
	}
}
