package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetReloadEnv is a YAML fleet whose databases are disposable physical
// databases on the designated test server, with the fleet reload owner
// registered on a fresh config controller.
type fleetReloadEnv struct {
	dsn   map[string]string
	admin *pgxpool.Pool
}

func newFleetReloadEnv(t *testing.T, names ...string) *fleetReloadEnv {
	t.Helper()
	admin := reloadAdminPool(t)
	preserveFleetRuntimeGlobals(t)
	oldCtx, oldPool, oldMeta := shutdownCtx, pool, globalMetaState
	env := &fleetReloadEnv{dsn: map[string]string{}, admin: admin}
	dbs := make([]config.DatabaseConfig, 0, len(names))
	for _, name := range names {
		env.dsn[name] = testdb.CreateDatabase(t, "reload_"+name)
		dbs = append(dbs, reloadDatabaseConfig(t, name, env.dsn[name]))
	}
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx, pool, globalMetaState = ctx, nil, nil
	cfg = reloadBaseConfig()
	cfg.Databases = dbs
	llmClient, llmMgr, configController = nil, nil, nil
	analyzeSem = make(chan struct{}, 1)
	fleetMgr = fleet.NewManager(cfg)
	initializeFleetBudget(names)
	boot := &fleetBootstrap{}
	for _, db := range dbs {
		boot.start(db)
	}
	configController = config.NewConfigController(cfg, nil, newTrustPolicyOwner())
	registerFleetDatabasesOwner(boot)
	t.Cleanup(func() {
		cancel()
		drainAllInstances(t)
		shutdownCtx, pool, globalMetaState = oldCtx, oldPool, oldMeta
	})
	return env
}

func reloadBaseConfig() *config.Config {
	base := config.DefaultConfig()
	base.Mode, base.Trust.Level = "fleet", "observation"
	base.Collector.IntervalSeconds, base.Analyzer.IntervalSeconds = 3600, 3600
	base.LLM.Enabled = false
	base.LLM.FleetTokenBudgetDaily = 10000
	return base
}

func drainAllInstances(t *testing.T) {
	t.Helper()
	if fleetMgr == nil {
		return
	}
	for _, inst := range fleetMgr.Instances() {
		if err := fleet.ShutdownInstance(context.Background(), inst); err != nil {
			t.Errorf("drain runtime %q: %v", inst.Name, err)
		}
	}
}

func reloadAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	return admin
}

// reloadDatabaseConfig is the normalized YAML entry for one test database.
func reloadDatabaseConfig(t *testing.T, name, dsn string) config.DatabaseConfig {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("test DSN port: %v", err)
	}
	password, _ := parsed.User.Password()
	return config.DatabaseConfig{
		Name: name, Host: parsed.Hostname(), Port: port,
		User: parsed.User.Username(), Password: password,
		Database: parsed.Path[1:], SSLMode: "disable", MaxConnections: 3,
		TrustLevel: "observation", ExecutionMode: "auto",
	}
}

func (env *fleetReloadEnv) config(t *testing.T, name string) config.DatabaseConfig {
	t.Helper()
	return reloadDatabaseConfig(t, name, env.dsn[name])
}

// reload publishes a mutated copy of the desired config the way the YAML
// watcher does.
func (env *fleetReloadEnv) reload(mutate func(*config.Config)) error {
	candidate := configController.Desired().Config
	mutate(candidate)
	return applyWatchedConfig(candidate)
}

func withoutDatabase(dbs []config.DatabaseConfig, name string) []config.DatabaseConfig {
	kept := make([]config.DatabaseConfig, 0, len(dbs))
	for _, db := range dbs {
		if db.Name != name {
			kept = append(kept, db)
		}
	}
	return kept
}

func databaseIndex(dbs []config.DatabaseConfig, name string) int {
	for i, db := range dbs {
		if db.Name == name {
			return i
		}
	}
	return -1
}

// pgSageConnections counts this sidecar's backends on one database.
func pgSageConnections(t *testing.T, admin *pgxpool.Pool, database string) int {
	t.Helper()
	var count int
	err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = $1 AND application_name = 'pg_sage'`, database).Scan(&count)
	if err != nil {
		t.Fatalf("count pg_sage connections: %v", err)
	}
	return count
}

func eventually(t *testing.T, timeout time.Duration, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, label)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// settledGoroutines waits for the goroutine count to fall to at most limit
// and returns the last count observed.
func settledGoroutines(limit int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		count := runtime.NumGoroutine()
		if count <= limit || time.Now().After(deadline) {
			return count
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func renderMetrics(t *testing.T) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handleMetrics(recorder, httptest.NewRequest("GET", "/metrics", nil))
	return recorder.Body.String()
}

func poolClosed(p *pgxpool.Pool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return p.Ping(ctx) != nil
}
