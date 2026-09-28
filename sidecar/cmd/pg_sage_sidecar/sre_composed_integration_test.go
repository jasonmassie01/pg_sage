package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M0 exit criterion: collector -> incident -> notify composes
// in standalone and fleet modes. Each test builds the per-database
// runtime pieces the sidecar wires (collector, rcaAdapter with its
// lock-chain fast path, shared notification dispatcher) against real
// PostgreSQL, creates a real blocking chain, and checks what reaches a
// webhook channel configured through sage.notification_*.

type hookSink struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newHookSink(t *testing.T) *hookSink {
	t.Helper()
	s := &hookSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.bodies = append(s.bodies, string(raw))
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(s.srv.Close)
	return s
}

// matching returns deliveries that contain every needle.
func (s *hookSink) matching(needles ...string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.bodies {
		ok := true
		for _, n := range needles {
			ok = ok && strings.Contains(b, n)
		}
		if ok {
			out = append(out, b)
		}
	}
	return out
}

func composedConfig(mode string) *config.Config {
	c := config.DefaultConfig()
	c.Mode = mode
	c.Collector.IntervalSeconds = 1
	c.RCA.Enabled = true
	c.RCA.LockChainIntervalSeconds = 60
	c.Analyzer.LockChain.MinBlockedThreshold = 1
	c.LLM.Enabled = false
	c.NotificationPolicy.AllowPrivateTargets = true
	return c
}

func installComposedGlobals(t *testing.T, c *config.Config) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	resetNotifyDispatchers(t)
	cfg = c
}

func openComposedPool(
	t *testing.T, ctx context.Context, dsn string,
) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// routeIncidentEvents points incident_* rules at the webhook sink.
func routeIncidentEvents(
	t *testing.T, ctx context.Context, control *pgxpool.Pool, sink *hookSink,
) {
	t.Helper()
	name := "sre-m0-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	cfgJSON, _ := json.Marshal(map[string]string{"webhook_url": sink.srv.URL})
	var id int
	if err := control.QueryRow(ctx, `INSERT INTO sage.notification_channels
		(name, type, config) VALUES ($1, 'slack', $2) RETURNING id`,
		name, cfgJSON).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = control.Exec(context.Background(),
			"DELETE FROM sage.notification_rules WHERE channel_id = $1", id)
		_, _ = control.Exec(context.Background(),
			"DELETE FROM sage.notification_log WHERE channel_id = $1", id)
		_, _ = control.Exec(context.Background(),
			"DELETE FROM sage.notification_channels WHERE id = $1", id)
	})
	for _, ev := range []string{"incident_detected", "incident_resolved"} {
		if _, err := control.Exec(ctx, `INSERT INTO sage.notification_rules
			(channel_id, event, min_severity) VALUES ($1, $2, 'info')`,
			id, ev); err != nil {
			t.Fatalf("insert rule %s: %v", ev, err)
		}
	}
}

type composedChain struct {
	holderPID int
	release   func()
}

// startComposedChain holds ACCESS SHARE idle in transaction, queues an
// ALTER TABLE behind it and a reader behind the ALTER (depth 2).
func startComposedChain(
	t *testing.T, ctx context.Context, dsn string, pool *pgxpool.Pool,
) *composedChain {
	t.Helper()
	ident := pgx.Identifier{"sre_m0_chain"}.Sanitize()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+ident+
		"; CREATE TABLE "+ident+" (id int)"); err != nil {
		t.Fatalf("chain table: %v", err)
	}
	dial := func() *pgx.Conn {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}
	holder := dial()
	cc := &composedChain{}
	for _, sql := range []string{"BEGIN", "SELECT count(*) FROM " + ident} {
		if _, err := holder.Exec(ctx, sql); err != nil {
			t.Fatalf("holder %s: %v", sql, err)
		}
	}
	_ = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&cc.holderPID)
	conns := []*pgx.Conn{holder}
	var wg sync.WaitGroup
	for i, sql := range []string{"ALTER TABLE " + ident + " ADD COLUMN v int",
		"SELECT count(*) FROM " + ident} {
		c := dial()
		conns = append(conns, c)
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Exec(context.Background(), sql) }()
		waitComposedWaiters(t, ctx, pool, i+1)
	}
	var once sync.Once
	cc.release = func() {
		once.Do(func() {
			_, _ = holder.Exec(context.Background(), "ROLLBACK")
			wg.Wait()
			for _, c := range conns {
				_ = c.Close(context.Background())
			}
			_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
		})
	}
	t.Cleanup(cc.release)
	return cc
}

func waitComposedWaiters(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int,
) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("blocking chain did not reach %d waiters", want)
}

// composedRuntime is one monitored database's wiring.
type composedRuntime struct {
	name    string
	pool    *pgxpool.Pool
	coll    *collector.Collector
	adapter *rcaAdapter
	eng     *rca.Engine
}

func startComposedRuntime(
	t *testing.T, ctx context.Context, name string,
	pool, control *pgxpool.Pool, llmClient *llm.Client,
) *composedRuntime {
	t.Helper()
	var version int
	_ = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version)
	rt := &composedRuntime{name: name, pool: pool}
	rt.coll = collector.New(pool, cfg, version, func(string, string, ...any) {})
	go rt.coll.Run(ctx)
	rt.eng = rca.NewEngine(&cfg.RCA, func(string, string, ...any) {})
	if llmClient != nil {
		rt.eng.WithLLM(llmClient)
	}
	rt.adapter = newRCAAdapter(ctx, rt.eng, pool, name,
		func(string, string, ...any) {})
	rt.eng.WithDispatcher(sharedNotifyDispatcher(control))
	for deadline := time.Now().Add(15 * time.Second); rt.coll.LatestSnapshot() == nil; {
		if time.Now().After(deadline) {
			t.Fatalf("%s: collector produced no snapshot", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", name)
	})
	return rt
}

// slowCycle is one analyzer cycle with no lock-chain findings.
func (rt *composedRuntime) slowCycle(t *testing.T, ctx context.Context) {
	t.Helper()
	rt.adapter.Analyze(rt.coll.LatestSnapshot(), rt.coll.PreviousSnapshot(),
		cfg, nil)
	if err := rt.adapter.PersistIncidents(ctx, rt.pool); err != nil {
		t.Fatalf("%s persist: %v", rt.name, err)
	}
}

func (rt *composedRuntime) fastTick(t *testing.T, ctx context.Context) {
	t.Helper()
	if rt.adapter.fastPath == nil {
		t.Fatalf("%s: lock-chain fast path was not started", rt.name)
	}
	if err := rt.adapter.fastPath.Tick(ctx); err != nil {
		t.Fatalf("%s fast tick: %v", rt.name, err)
	}
}

func (rt *composedRuntime) openLockIncidents(
	t *testing.T, ctx context.Context,
) []string {
	t.Helper()
	rows, err := rt.pool.Query(ctx, `SELECT causal_chain::text
		FROM sage.incidents WHERE database_name = $1 AND resolved_at IS NULL
		AND signal_ids = ARRAY['lock_contention']`, rt.name)
	if err != nil {
		t.Fatalf("query incidents: %v", err)
	}
	defer rows.Close()
	var chains []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		chains = append(chains, c)
	}
	return chains
}

func narratorLLM(t *testing.T) *llm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": `{"summary": ` +
					`"An idle-in-transaction session blocks a queued DDL.", ` +
					`"evidence_ids": ["E2"]}`}}},
			"usage": map[string]int{"total_tokens": 50},
		})
	}))
	t.Cleanup(srv.Close)
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL,
		APIKey: "k", Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 100000},
		func(string, string, ...any) {})
}

func resolveLockIncident(t *testing.T, ctx context.Context, rt *composedRuntime) {
	t.Helper()
	for i := 0; i < 8 && len(rt.openLockIncidents(t, ctx)) > 0; i++ {
		rt.slowCycle(t, ctx)
	}
	if n := len(rt.openLockIncidents(t, ctx)); n != 0 {
		t.Fatalf("%s: lock incident still open after the chain cleared", rt.name)
	}
}

func TestComposedSRE_StandaloneCollectorIncidentNotify(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	c := composedConfig("standalone")
	c.RCA.NarrationEnabled = true
	installComposedGlobals(t, c)
	pool := openComposedPool(t, ctx, dsn)
	sink := newHookSink(t)
	routeIncidentEvents(t, ctx, pool, sink)
	rt := startComposedRuntime(t, ctx, "standalone_db", pool, pool, narratorLLM(t))

	rt.slowCycle(t, ctx) // first analyzer cycle also starts the fast path
	if rt.adapter.fastPath.Interval() != 60*time.Second {
		t.Fatalf("fast path interval = %s, want 60s", rt.adapter.fastPath.Interval())
	}
	chain := startComposedChain(t, ctx, dsn, pool)
	rt.fastTick(t, ctx)

	chains := rt.openLockIncidents(t, ctx)
	if len(chains) != 1 || !strings.Contains(chains[0],
		fmt.Sprintf(`"pid": %d`, chain.holderPID)) {
		t.Fatalf("incident chain %v lacks holder pid %d", chains, chain.holderPID)
	}
	if !strings.Contains(chains[0], `"backend_start"`) {
		t.Fatalf("incident chain lacks backend_start: %s", chains[0])
	}
	det := sink.matching("Incident detected", "Database: standalone_db",
		"lock_contention", "Summary (LLM, cites E2)")
	if len(det) != 1 {
		t.Fatalf("detected deliveries = %d, want 1 narrated delivery; all: %v",
			len(det), sink.matching())
	}

	chain.release()
	resolveLockIncident(t, ctx, rt)
	if n := len(sink.matching("Incident resolved", "Database: standalone_db",
		"lock_contention")); n != 1 {
		t.Fatalf("resolved deliveries = %d, want 1", n)
	}
}

// createFleetDatabase creates a second monitored database on the same
// cluster so per-database isolation is tested against real sessions.
func createFleetDatabase(
	t *testing.T, ctx context.Context, admin *pgxpool.Pool, dsn string,
) string {
	t.Helper()
	name := fmt.Sprintf("sre_m0_fleet_b_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create fleet database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect fleet database: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx,
		"CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("fleet database pg_stat_statements: %v", err)
	}
	return u.String()
}

func TestComposedSRE_FleetCollectorIncidentNotifyIsolated(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	installComposedGlobals(t, composedConfig("fleet"))
	control := openComposedPool(t, ctx, dsn) // fleet primary = control pool
	dsnB := createFleetDatabase(t, ctx, control, dsn)
	poolB := openComposedPool(t, ctx, dsnB)
	sink := newHookSink(t)
	routeIncidentEvents(t, ctx, control, sink)

	rtA := startComposedRuntime(t, ctx, "fleet_a", control, control, nil)
	rtB := startComposedRuntime(t, ctx, "fleet_b", poolB, control, nil)
	rtA.slowCycle(t, ctx)
	rtB.slowCycle(t, ctx)

	chain := startComposedChain(t, ctx, dsnB, poolB) // chain only in B
	rtA.fastTick(t, ctx)
	rtB.fastTick(t, ctx)

	if n := len(rtA.openLockIncidents(t, ctx)); n != 0 {
		t.Fatalf("fleet_a opened %d lock incidents from fleet_b's sessions", n)
	}
	chainsB := rtB.openLockIncidents(t, ctx)
	if len(chainsB) != 1 || !strings.Contains(chainsB[0],
		fmt.Sprintf(`"pid": %d`, chain.holderPID)) {
		t.Fatalf("fleet_b incidents = %v, want one naming pid %d",
			chainsB, chain.holderPID)
	}
	if n := len(sink.matching("Incident detected", "Database: fleet_a",
		"lock_contention")); n != 0 {
		t.Fatalf("fleet_a notified %d lock incidents", n)
	}
	detB := sink.matching("Incident detected", "Database: fleet_b",
		"lock_contention", "Summary (deterministic)")
	if len(detB) != 1 {
		t.Fatalf("fleet_b detected deliveries = %d, want 1 deterministic; all: %v",
			len(detB), sink.matching())
	}

	chain.release()
	resolveLockIncident(t, ctx, rtB)
	if n := len(sink.matching("Incident resolved", "Database: fleet_b",
		"lock_contention")); n != 1 {
		t.Fatalf("fleet_b resolved deliveries = %d, want 1", n)
	}
}
