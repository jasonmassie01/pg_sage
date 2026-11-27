package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// observability-08: the separate /metrics listener binds synchronously; a
// bind failure is an ERROR, pg_sage_metrics_listener_up 0 and a nil server
// that shutdown tolerates; an empty prometheus.listen_addr opens nothing;
// prometheus.serve_on_api mounts the guarded route on the API listener.

// No concurrent-access tests for startPrometheusServer: main calls it once.
// The listener state it records is an atomic, read by concurrent scrapes
// (covered by the API route's concurrent scrape test).

func preserveListenerState(t *testing.T) {
	t.Helper()
	old := metricsListenerState.Load()
	t.Cleanup(func() { metricsListenerState.Store(old) })
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}
	return addr
}

func listenerGauge(t *testing.T) string {
	t.Helper()
	body := scrapeMetrics(t)
	families := parseExposition(t, body)
	fam, ok := families["pg_sage_metrics_listener_up"]
	if !ok {
		return "absent"
	}
	if len(fam.GetMetric()) != 1 {
		t.Fatalf("pg_sage_metrics_listener_up has %d samples, want 1", len(fam.GetMetric()))
	}
	if fam.GetMetric()[0].GetGauge().GetValue() == 1 {
		return "1"
	}
	return "0"
}

func TestPrometheusListenerServesOnAFreePort(t *testing.T) {
	preserveValueGlobals(t)
	preserveListenerState(t)
	cfg = testConfig()
	addr := freeAddr(t)
	srv := startPrometheusServer(addr)
	if srv == nil {
		t.Fatal("startPrometheusServer returned nil on a free port")
	}
	t.Cleanup(func() { shutdownPrometheus(context.Background(), srv) })
	// Bound before return: the first request needs no retry loop.
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "pg_sage_info") {
		t.Fatalf("status %d body %q, want the exposition", resp.StatusCode, body)
	}
	if got := listenerGauge(t); got != "1" {
		t.Fatalf("pg_sage_metrics_listener_up = %s, want 1", got)
	}
}

// Error propagation: an occupied port (postgres_exporter's 9187, say) is an
// ERROR log and a 0 gauge, not a silently missing listener.
func TestPrometheusListenerOccupiedPortReportsDown(t *testing.T) {
	preserveValueGlobals(t)
	preserveListenerState(t)
	cfg = testConfig()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	addr := busy.Addr().String()

	var srv *http.Server
	logged := captureStderr(t, func() { srv = startPrometheusServer(addr) })
	if srv != nil {
		t.Fatal("startPrometheusServer returned a server for an occupied port")
	}
	if !strings.Contains(logged, "[ERROR] [prometheus]") || !strings.Contains(logged, addr) ||
		!strings.Contains(logged, "prometheus.serve_on_api") {
		t.Fatalf("log = %q, want an ERROR naming %s and the serve_on_api remedy", logged, addr)
	}
	if got := listenerGauge(t); got != "0" {
		t.Fatalf("pg_sage_metrics_listener_up = %s, want 0", got)
	}
	// Shutdown with the nil server must not panic.
	shutdownPrometheus(context.Background(), srv)
}

// Boundary: an empty listen address opens no socket and exports no gauge.
func TestPrometheusListenerEmptyAddrOpensNoSocket(t *testing.T) {
	preserveValueGlobals(t)
	preserveListenerState(t)
	cfg = testConfig()
	probe := freeAddr(t)
	logged := captureStderr(t, func() {
		if srv := startPrometheusServer(""); srv != nil {
			t.Error("an empty listen address returned a server")
		}
	})
	if !strings.Contains(logged, "listen_addr is empty") {
		t.Errorf("log = %q, want a line saying the separate listener is off", logged)
	}
	ln, err := net.Listen("tcp", probe)
	if err != nil {
		t.Fatalf("address %s is taken after startPrometheusServer(\"\"): %v", probe, err)
	}
	_ = ln.Close()
	// The default address too, when nothing else on this machine holds it.
	if ln, err := net.Listen("tcp", config.DefaultPrometheusListenAddr); err == nil {
		_ = ln.Close()
	} else {
		t.Logf("default %s is held by another process; the free-port probe "+
			"above covers the check", config.DefaultPrometheusListenAddr)
	}
	if got := listenerGauge(t); got != "absent" {
		t.Fatalf("pg_sage_metrics_listener_up = %s, want absent when no listener is configured",
			got)
	}
}

func TestPrometheusListenerShutdownStopsServing(t *testing.T) {
	preserveValueGlobals(t)
	preserveListenerState(t)
	cfg = testConfig()
	addr := freeAddr(t)
	srv := startPrometheusServer(addr)
	if srv == nil {
		t.Fatal("no server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownPrometheus(ctx, srv)
	if _, err := http.Get("http://" + addr + "/metrics"); err == nil {
		t.Fatal("the listener still answers after shutdown")
	}
}

// Nil/empty: serve_on_api without a control pool (no token store) refuses
// to start, naming the key.
func TestPrometheusListenerServeOnAPINeedsATokenStore(t *testing.T) {
	c := testConfig()
	c.Prometheus.ServeOnAPI = true
	err := checkMetricsOnAPI(c, nil)
	if err == nil || !strings.Contains(err.Error(), "prometheus.serve_on_api") {
		t.Fatalf("checkMetricsOnAPI(no pool) = %v, want an error naming "+
			"prometheus.serve_on_api", err)
	}
	p, perr := pgxpool.New(context.Background(), "postgres://u@127.0.0.1:1/db")
	if perr != nil {
		t.Fatalf("lazy pool: %v", perr)
	}
	t.Cleanup(p.Close)
	if err := checkMetricsOnAPI(c, p); err != nil {
		t.Fatalf("checkMetricsOnAPI(pool) = %v, want nil", err)
	}
	c.Prometheus.ServeOnAPI = false
	if err := checkMetricsOnAPI(c, nil); err != nil {
		t.Fatalf("serve_on_api off: %v, want nil", err)
	}
	if err := checkMetricsOnAPI(nil, nil); err != nil {
		t.Fatalf("nil config: %v, want nil", err)
	}
}

func wiredScrape(t *testing.T, p WireParams, header map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	h := testWireRouter(p).Handler
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPrometheusListenerWiredRouteIsGuarded(t *testing.T) {
	preserveValueGlobals(t)
	c := testConfig()
	cfg = c
	off := wiredScrape(t, WireParams{Cfg: c, FleetMgr: testFleetMgr(c)}, nil)
	if strings.Contains(off.Body.String(), "pg_sage_info") {
		t.Fatal("serve_on_api off served the exposition on the API listener")
	}
	c.Prometheus.ServeOnAPI = true
	on := wiredScrape(t, WireParams{Cfg: c, FleetMgr: testFleetMgr(c)}, nil)
	if on.Code != http.StatusUnauthorized || strings.Contains(on.Body.String(), "pg_sage_") {
		t.Fatalf("unauthenticated scrape: %d %q, want 401 without metric names",
			on.Code, on.Body.String())
	}
	app := &auth.DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		AdminEmail: "owner@example.com"}
	dbx := wiredScrape(t, WireParams{Cfg: c, FleetMgr: testFleetMgr(c),
		DatabricksAppAuth: app}, map[string]string{"Authorization": "Bearer x"})
	if dbx.Code != http.StatusUnauthorized || !strings.Contains(dbx.Body.String(), "Databricks") {
		t.Fatalf("Databricks scrape without platform identity: %d %q, want the "+
			"platform's 401", dbx.Code, dbx.Body.String())
	}
}

// The route's handler is the same handleMetrics as the separate listener.
func TestPrometheusListenerAPIRouteRendersTheExposition(t *testing.T) {
	preserveValueGlobals(t)
	c := testConfig()
	c.Prometheus.ServeOnAPI = true
	cfg = c
	route := apiMetricsRoute(WireParams{Cfg: c}, nil)
	if route == nil || route.Handler == nil {
		t.Fatal("serve_on_api built no metrics route")
	}
	w := httptest.NewRecorder()
	route.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	families := parseExposition(t, w.Body.String())
	if _, ok := families["pg_sage_info"]; !ok {
		t.Fatalf("API route body lacks pg_sage_info: %q", w.Body.String())
	}
	c.Prometheus.ServeOnAPI = false
	if apiMetricsRoute(WireParams{Cfg: c}, nil) != nil {
		t.Fatal("serve_on_api off still built a metrics route")
	}
}
