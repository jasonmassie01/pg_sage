package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// In-process characterization of the main.go helpers whose coverage was thin
// before the split: API/Prometheus server construction, the standalone
// metrics sections, the shutdown drains and the trusted-proxy parser.

func preserveMainWiringGlobals(t *testing.T) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldPool, oldAPI, oldExec := pool, apiServer, exec
	oldColl, oldAnal := coll, anal
	t.Cleanup(func() {
		pool, apiServer, exec = oldPool, oldAPI, oldExec
		coll, anal = oldColl, oldAnal
	})
}

func TestCharStartAPIServer_RefusesWithoutRateLimiter(t *testing.T) {
	preserveMainWiringGlobals(t)
	cfg = config.DefaultConfig()
	apiServer = nil
	logs := captureStderr(t, func() { startAPIServer(nil) })
	if apiServer != nil {
		t.Fatal("API server built without a rate limiter")
	}
	if !strings.Contains(logs, "[ERROR] [api] rate limiter unavailable; refusing to start") {
		t.Fatalf("refusal not logged: %q", logs)
	}
}

func TestCharPrometheusServer_TimeoutsAndMetricsRoute(t *testing.T) {
	preserveMainWiringGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	pool, fleetMgr = nil, fleet.NewManager(cfg)
	srv := startPrometheusServer("127.0.0.1:0")
	t.Cleanup(func() { _ = srv.Close() })
	if srv.Addr != "127.0.0.1:0" || srv.ReadHeaderTimeout != 10*time.Second ||
		srv.ReadTimeout != 10*time.Second || srv.WriteTimeout != 10*time.Second ||
		srv.IdleTimeout != 60*time.Second {
		t.Fatalf("prometheus server = %+v", srv)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK ||
		rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("/metrics = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "pg_sage_fleet_databases 0") ||
		!strings.Contains(rec.Body.String(), "pg_sage_connection_up 0") {
		t.Fatalf("fleet metrics body:\n%s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/other = %d, want 404", rec.Code)
	}
}

func TestCharHandleMetrics_StandaloneSections(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	preserveMainWiringGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Mode, cfg.LLM.Enabled, cfg.LLM.TokenBudgetDaily = "standalone", false, 4321
	pool = openComposedPool(t, ctx, dsn)
	fleetMgr, coll, anal, llmMgr, fleetLLMBudget = nil, nil, nil, nil, nil
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	var findingID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ('covering_index', 'info', 'table', 'public.char_metrics', 't', '{}', 'r')
		RETURNING id`).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings WHERE id = $1`,
			findingID)
	})
	rec := httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`pg_sage_info{version="dev",mode="standalone"} 1`, "pg_sage_connection_up 1",
		"# TYPE pg_sage_findings_total gauge", "pg_sage_llm_enabled 0",
		"pg_sage_llm_circuit_open 0", "pg_sage_llm_tokens_used_today 0",
		"pg_sage_llm_tokens_budget_daily 4321",
		`pg_sage_optimizer_recommendations_total{category="covering_index"} `,
		"pg_sage_optimizer_enabled 0", "pg_sage_database_size_bytes ",
		`pg_sage_connections_total{state=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("standalone metrics lack %q", want)
		}
	}
	if strings.Contains(body, "pg_sage_fleet_databases") ||
		strings.Contains(body, "pg_sage_collector_last_run_timestamp") {
		t.Errorf("standalone metrics include fleet/collector sections:\n%s", body)
	}
}

func TestCharShutdownExecutors_DrainsStandaloneAndFleetExecutors(t *testing.T) {
	preserveMainWiringGlobals(t)
	cfg = config.DefaultConfig()
	exec, fleetMgr = nil, nil
	shutdownExecutors(context.Background()) // nothing registered: no-op
	exec = executor.New(nil, cfg, time.Time{}, logStructuredWrapper)
	fleetMgr = fleet.NewManager(cfg)
	fleetExec := executor.New(nil, cfg, time.Time{}, logStructuredWrapper)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "alpha", Executor: fleetExec,
		Status: &fleet.InstanceStatus{}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logs := captureStderr(t, func() { shutdownExecutors(ctx) })
	if logs != "" {
		t.Fatalf("idle executors failed to shut down: %q", logs)
	}
	for _, e := range []*executor.Executor{exec, fleetExec} {
		if err := e.Shutdown(ctx); err != nil {
			t.Fatalf("executor not reusable for a second Shutdown: %v", err)
		}
	}
}

func TestCharDrainRuntimeWorkers_WarnsOnDeadline(t *testing.T) {
	preserveMainWiringGlobals(t)
	cfg = config.DefaultConfig()
	fleetMgr = nil
	drainRuntimeWorkers(context.Background()) // no fleet: no-op
	fleetMgr = fleet.NewManager(cfg)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "idle",
		Workers: &sync.WaitGroup{}, Status: &fleet.InstanceStatus{}})
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "no-workers",
		Status: &fleet.InstanceStatus{}})
	if logs := captureStderr(t, func() { drainRuntimeWorkers(context.Background()) }); logs != "" {
		t.Fatalf("drained runtimes warned: %q", logs)
	}
	stuck := &sync.WaitGroup{}
	stuck.Add(1)
	t.Cleanup(stuck.Done)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "stuck", Workers: stuck,
		Status: &fleet.InstanceStatus{}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	logs := captureStderr(t, func() { drainRuntimeWorkers(ctx) })
	// Once the shared deadline passes, every remaining runtime is reported,
	// so only the stuck one is guaranteed (map order decides the rest).
	if !strings.Contains(logs, `db "stuck": runtime workers exceeded shutdown deadline`) ||
		strings.Contains(logs, `"no-workers"`) {
		t.Fatalf("drain logs = %q", logs)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain ignored its deadline: %s", elapsed)
	}
}

func TestCharTrustedProxyNets_ParsesAndSkipsInvalidEntries(t *testing.T) {
	var logs string
	var nets []string
	logs = captureStderr(t, func() {
		for _, n := range buildTrustedProxyNets([]string{
			" 10.0.0.1 ", "", "::1", "192.168.0.0/16", "not-an-ip", "10.0.0.0/33",
		}) {
			nets = append(nets, n.String())
		}
	})
	if strings.Join(nets, ",") != "10.0.0.1/32,::1/128,192.168.0.0/16" {
		t.Fatalf("nets = %v", nets)
	}
	if !strings.Contains(logs, `ignoring invalid IP "not-an-ip"`) ||
		!strings.Contains(logs, `ignoring invalid CIDR "10.0.0.0/33"`) {
		t.Fatalf("invalid entries not logged: %q", logs)
	}
	defaults := buildTrustedProxyNets(nil)
	if len(defaults) != 2 || defaults[0].String() != "127.0.0.1/32" ||
		defaults[1].String() != "::1/128" {
		t.Fatalf("default trusted proxies = %v", defaults)
	}
}

func TestCharEnvOrDefault(t *testing.T) {
	t.Setenv("PG_SAGE_CHAR_ENV", "")
	if got := envOrDefault("PG_SAGE_CHAR_ENV", "fallback"); got != "fallback" {
		t.Fatalf("empty env = %q", got)
	}
	t.Setenv("PG_SAGE_CHAR_ENV", "set")
	if got := envOrDefault("PG_SAGE_CHAR_ENV", "fallback"); got != "set" {
		t.Fatalf("set env = %q", got)
	}
}
