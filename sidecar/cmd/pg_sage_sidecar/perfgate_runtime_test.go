//go:build perfgate

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// perfCounters is one reading of everything the gate measures.
type perfCounters struct {
	tables     perfgate.TableStats
	statements []perfgate.Statement
	timeouts   []string
}

// runPerfRuntime runs the standalone runtime through a warmup phase
// (startup, first cycles, keyframes, baselines) and a steady phase, and
// returns both phases' measurements with generic plans attached.
func runPerfRuntime(
	t *testing.T, ctx context.Context, harness *pgxpool.Pool, dsn string,
	scale perfgate.Scale, timing perfgate.Timing,
) []perfgate.Phase {
	t.Helper()
	preserveParityGlobals(t, perfConfig(t, dsn, timing))
	cfg.Mode = "standalone"
	// pg_sage no longer silences pg_stat_statements on its own sessions
	// (perf v1.8.3): the gate measures the shipped behaviour as is.
	logs := capturePerfLogs(t)
	session := perfAPISession(t, ctx, harness)
	monitored, err := connectMonitoredDB(dsn, cfg.Postgres.MaxConnections)
	if err != nil {
		t.Fatalf("connect monitored pool: %v", err)
	}
	pool = monitored
	cloudEnvironment = detectCloudEnvironment()
	cfg.CloudEnvironment = cloudEnvironment

	base := readPerfCounters(t, ctx, harness, logs, true)
	initStandalone()
	router := perfRouter(t)
	stopWorkload := startPerfWorkload(t, dsn, scale.HotTables())
	time.Sleep(timing.Warmup)
	warm := readPerfCounters(t, ctx, harness, logs, true)

	steadyStart := time.Now()
	time.Sleep(timing.Window / 2)
	endpoints := callPerfEndpoints(router, session)
	time.Sleep(time.Until(steadyStart.Add(timing.Window)))
	stopWorkload()
	stopPerfRuntime(t, monitored)
	end := readPerfCounters(t, ctx, harness, logs, false)

	warmup := perfPhase("warmup", false, timing.Warmup, 0, base, warm)
	steady := perfPhase("steady", true, timing.Window, timing.Cycles(), warm, end)
	steady.Endpoints = endpoints
	explainPerfPhases(t, ctx, harness, &warmup, &steady)
	return []perfgate.Phase{warmup, steady}
}

// perfFakeModel is an OpenAI-compatible endpoint that answers every
// tuning case with no proposal.
func perfFakeModel(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant",` +
			`"content":"{\"proposals\":[]}"},"finish_reason":"stop"}],` +
			`"usage":{"total_tokens":100}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// perfConfig is the shipped default configuration with the general LLM off and
// every periodic component compressed to one interval, so each runs about
// once per collector cycle in the window.
func perfConfig(t *testing.T, dsn string, timing perfgate.Timing) *config.Config {
	t.Helper()
	conn, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	c := config.DefaultConfig()
	// Every general LLM feature stays off (no general endpoint). The tuning
	// agent (roadmap 2.2) runs on the dedicated optimizer client against a
	// fake model, so the gate measures its reads: it must add no sequential
	// scan and no per-table query loop.
	c.LLM.Enabled, c.LLM.Endpoint, c.LLM.APIKey = true, "", ""
	c.LLM.OptimizerLLM = config.OptimizerLLMConfig{Enabled: true,
		Endpoint: perfFakeModel(t), APIKey: "perf-fixture", Model: "fake",
		TimeoutSeconds: 5, TokenBudgetDaily: 10_000_000, MaxOutputTokens: 4096}
	secs := int(timing.Interval / time.Second)
	c.Collector.IntervalSeconds, c.Analyzer.IntervalSeconds = secs, secs
	c.SRE.Runways.IntervalSeconds, c.SRE.Runways.SequenceIntervalSeconds = secs, secs
	c.SRE.TriggerIntervalSeconds = secs
	c.Postgres = config.PostgresConfig{
		Host: conn.Host, Port: int(conn.Port), User: conn.User,
		Password: conn.Password, Database: conn.Database,
		SSLMode: "disable", MaxConnections: 6,
	}
	return c
}

func readPerfCounters(
	t *testing.T, ctx context.Context, harness *pgxpool.Pool, logs *perfLogCapture,
	reset bool,
) perfCounters {
	t.Helper()
	tables, err := perfgate.ReadTableStats(ctx, harness)
	if err != nil {
		t.Fatalf("read table statistics: %v", err)
	}
	stmts, err := perfgate.ReadStatements(ctx, harness, harness.Config().ConnConfig.Database)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	if reset {
		if err := perfgate.ResetStatements(ctx, harness); err != nil {
			t.Fatalf("reset pg_stat_statements: %v", err)
		}
	}
	return perfCounters{tables: tables, statements: stmts, timeouts: logs.drain()}
}

// perfPhase is the difference between two readings. Statements are
// reset at the start of each phase, so the later reading is the phase's.
func perfPhase(
	name string, steady bool, window time.Duration, cycles int, from, to perfCounters,
) perfgate.Phase {
	return perfgate.Phase{
		Name: name, Steady: steady, Window: window, Cycles: cycles,
		Tables: to.tables.Delta(from.tables), Statements: to.statements,
		Timeouts: to.timeouts,
	}
}

// stopPerfRuntime drains the runtime and closes its pool: backends flush
// their table statistics when they exit.
func stopPerfRuntime(t *testing.T, monitored *pgxpool.Pool) {
	t.Helper()
	drainParityRuntimes(t)
	monitored.Close()
	time.Sleep(1500 * time.Millisecond)
}

func explainPerfPhases(
	t *testing.T, ctx context.Context, harness *pgxpool.Pool, phases ...*perfgate.Phase,
) {
	t.Helper()
	for _, p := range phases {
		plans, err := perfgate.ExplainStatements(ctx, harness, p.Statements)
		if err != nil {
			t.Fatalf("explain %s statements: %v", p.Name, err)
		}
		p.Plans = plans
	}
}

func perfHarnessPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	pc.MaxConns = 4
	pc.ConnConfig.RuntimeParams["application_name"] = "perfgate_harness"
	p, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("harness pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}
