package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// observability-03: a blackholed database used the whole 5 s scrape
// budget (ping, value and SLO reads all ran at scrape time), so the scrape
// stalled and every database read as down. Scrapes now render snapshots.
// This test compiles against the old code and fails there on the timing.
func TestScrapeWithBlackholedDatabaseReturnsPromptly(t *testing.T) {
	oldCfg, oldPool, oldMgr := cfg, pool, fleetMgr
	t.Cleanup(func() { cfg, pool, fleetMgr = oldCfg, oldPool, oldMgr })
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	pool = nil
	fleetMgr = fleet.NewManager(cfg)
	pcfg, err := pgxpool.ParseConfig("postgres://sage@192.0.2.1:5432/hung?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	pcfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("test finished")
		}
	}
	blackholed, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(blackholed.Close)
	t.Cleanup(func() { close(release) })
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "hung", Pool: blackholed,
		Config: config.DatabaseConfig{Name: "hung"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()}})

	start := time.Now()
	w := httptest.NewRecorder()
	handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("scrape with a blackholed database took %v, want under 1s", elapsed)
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(),
		"pg_sage_fleet_databases 1\n") {
		t.Fatalf("scrape = %d:\n%s", w.Code, w.Body.String())
	}
}

// CHECK-07: the scrape handler's file holds no database call at all.
func TestPrometheusSourceHasNoDatabaseCalls(t *testing.T) {
	src, err := os.ReadFile("prometheus.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	for _, call := range []string{"pool.Query", "QueryRow", "Ping(", ".Exec("} {
		if strings.Contains(string(src), call) {
			t.Errorf("prometheus.go calls %s on the scrape path", call)
		}
	}
}

// Happy path: ten scrapes in a row touch the database zero times, yet
// render the database-derived series of the last refresh.
func TestScrapeIssuesNoDatabaseIO(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	preserveMainWiringGlobals(t)
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	counter := &ioCounter{}
	traced := tracedComposedPool(t, dsn, counter)
	cfg = config.DefaultConfig()
	cfg.Mode = "standalone"
	pool, coll, anal, llmClient = traced, nil, nil, nil
	fleetMgr = fleet.NewManager(cfg)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "solo", Pool: traced,
		Config: config.DatabaseConfig{Name: "solo"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()}})
	registerStandaloneMetrics(t, "solo", traced)
	if counter.n.Load() == 0 {
		t.Fatal("the refresh issued no queries; the tracer is not wired")
	}
	counter.n.Store(0)
	var out string
	for i := 0; i < 10; i++ {
		out = scrapeMetrics(t)
	}
	if n := counter.n.Load(); n != 0 {
		t.Fatalf("10 scrapes issued %d database round trips, want 0", n)
	}
	for _, want := range []string{
		"pg_sage_connection_up 1\n",
		`pg_sage_value_metrics_up{database="solo"} 1`,
		`pg_sage_metrics_refresh_success{database="solo",source="optimizer"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape lacks %q:\n%s", want, out)
		}
	}
}

func tracedComposedPool(t *testing.T, dsn string, counter *ioCounter) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	openComposedPool(t, ctx, dsn) // bootstraps the schema
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.Tracer = counter
	p, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Integration: pg_stat_statements sees the refresher's tagged statements
// once per refresh, the same whether nobody scrapes or 50 scrapes run.
func TestRefresherStatementsFollowTheCadenceNotTheScrapes(t *testing.T) {
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	installValueFleet(nil)
	p, sage := statementsPool(t)
	m := registerValueRefresher(t, "cadence", sage)
	for _, scrapes := range []int{0, 50} {
		resetDatabaseStatements(t, p)
		for i := 0; i < 3; i++ {
			m.refresh(context.Background())
			for j := 0; j < scrapes/3+1 && scrapes > 0; j++ {
				scrapeMetrics(t)
			}
		}
		if got := taggedCalls(t, p, "incident_avoided"); got != 3 {
			t.Errorf("%d scrapes: incident counts ran %d times, want 3 (one per refresh)",
				scrapes, got)
		}
		if got := taggedCalls(t, p, "pg_control_system"); got != 3 {
			t.Errorf("%d scrapes: identity ran %d times, want 3", scrapes, got)
		}
	}
	resetDatabaseStatements(t, p)
	for i := 0; i < 20; i++ {
		scrapeMetrics(t)
	}
	if got := taggedCalls(t, p, ""); got != 0 {
		t.Fatalf("scrapes alone ran %d tagged statements, want 0", got)
	}
}

// statementsPool returns a plain pool for reading and resetting
// pg_stat_statements (its unqualified name resolves in public) and a pool
// configured like the runtime's for the refresher: the runtime's tagger
// moves each statement's tag after its first keyword, which PostgreSQL 18
// needs to keep it (a leading comment is dropped from the recorded text).
func statementsPool(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dsn := testdb.CreateDatabase(t, "metrics_cadence")
	p := openComposedPool(t, ctx, dsn)
	if _, err := p.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements unavailable on the test server: %v", err)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM pg_stat_statements").Scan(&n); err != nil {
		t.Skipf("pg_stat_statements not preloaded on the test server: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	sage, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the runtime-configured pool: %v", err)
	}
	t.Cleanup(sage.Close)
	return p, sage
}

func resetDatabaseStatements(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	if _, err := p.Exec(context.Background(), `SELECT pg_stat_statements_reset(0,
		(SELECT oid FROM pg_database WHERE datname = current_database()), 0)`); err != nil {
		t.Fatalf("reset pg_stat_statements: %v", err)
	}
}

// taggedCalls sums the calls of this database's /* pg_sage */ statements
// whose text contains fragment. The tag may sit after the first keyword
// (selfmonitor places it there; PostgreSQL 18 drops a leading comment).
func taggedCalls(t *testing.T, p *pgxpool.Pool, fragment string) int64 {
	t.Helper()
	var calls int64
	err := p.QueryRow(context.Background(), `SELECT COALESCE(sum(calls), 0)::bigint
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND strpos(query, '/* pg_sage */') > 0 AND strpos(query, $1) > 0`, fragment).Scan(&calls)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	return calls
}
