package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// States of the separate /metrics listener (observability-08).
const (
	listenerOff  int32 = iota // prometheus.listen_addr is empty
	listenerDown              // configured, but binding (or serving) failed
	listenerUp                // bound and serving
)

// metricsListenerState is read by every scrape and written at startup.
var metricsListenerState atomic.Int32

// startPrometheusServer binds the separate /metrics listener before it
// returns. An empty addr opens none. A bind failure is logged as an ERROR,
// exported as pg_sage_metrics_listener_up 0, and returns nil: the sidecar
// keeps running, and shutdown tolerates the nil server.
func startPrometheusServer(addr string) *http.Server {
	if addr == "" {
		metricsListenerState.Store(listenerOff)
		logInfo("prometheus", "separate listener off: prometheus.listen_addr is empty")
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		metricsListenerState.Store(listenerDown)
		logError("prometheus", "cannot listen on %s: %v; /metrics is not served there. "+
			"Free the port, set "+
			"prometheus.listen_addr or SAGE_PROMETHEUS_ADDR to a free address, or set "+
			"prometheus.serve_on_api: true", addr, err)
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsListenerState.Store(listenerUp)
	logInfo("prometheus", "listening on %s", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			metricsListenerState.Store(listenerDown)
			logError("prometheus", "server error on %s: %v", addr, err)
		}
	}()
	return srv
}

// shutdownPrometheus stops the separate listener; nil (none, or a failed
// bind) is a no-op.
func shutdownPrometheus(ctx context.Context, srv *http.Server) {
	if srv == nil {
		return
	}
	if err := srv.Shutdown(ctx); err != nil {
		logWarn("shutdown", "Prometheus server: %v", err)
	}
}

// writeMetricsListenerMetric reports the separate listener when one is
// configured; with prometheus.listen_addr empty the family is absent.
func writeMetricsListenerMetric(b *strings.Builder) {
	state := metricsListenerState.Load()
	if state == listenerOff {
		return
	}
	up := 0
	if state == listenerUp {
		up = 1
	}
	metricsreg.Header(b, "pg_sage_metrics_listener_up")
	fmt.Fprintf(b, "pg_sage_metrics_listener_up %d\n\n", up)
}

// checkMetricsOnAPI refuses prometheus.serve_on_api without a control
// database: no token store (or platform account store) could authorize a
// scrape.
func checkMetricsOnAPI(c *config.Config, authPool *pgxpool.Pool) error {
	if c == nil || !c.Prometheus.ServeOnAPI || authPool != nil {
		return nil
	}
	return errors.New("prometheus.serve_on_api needs a control database to check " +
		"metrics tokens, and none is connected: connect the monitored or metadata " +
		"database, or set prometheus.serve_on_api: false")
}

// apiMetricsRoute is the GET /metrics route of the API listener, nil when
// prometheus.serve_on_api is off. The rate limiter, and in a Databricks App
// the platform identity, run before the credential check.
func apiMetricsRoute(p WireParams, authPool *pgxpool.Pool) *api.MetricsRoute {
	if p.Cfg == nil || !p.Cfg.Prometheus.ServeOnAPI {
		return nil
	}
	route := &api.MetricsRoute{Handler: http.HandlerFunc(handleMetrics)}
	if p.RateLimiter != nil {
		route.Middlewares = append(route.Middlewares, apiRateLimit(p))
	}
	if p.DatabricksAppAuth != nil {
		route.Middlewares = append(route.Middlewares,
			api.DatabricksAppAuthMiddleware(authPool, *p.DatabricksAppAuth))
	}
	return route
}

// metricsWhere names where /metrics is served, for the startup line.
func metricsWhere(p config.PrometheusConfig) string {
	where := p.ListenAddr
	if !p.SeparateListener() {
		where = "off"
	}
	if p.ServeOnAPI {
		where += " (+ /metrics on the API listener)"
	}
	return where
}
