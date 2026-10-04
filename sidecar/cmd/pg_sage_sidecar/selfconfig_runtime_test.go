package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
	"github.com/pg-sage/sidecar/internal/store"
)

// Self-configuration in the runtime: each database derives its settings at
// startup (before any worker reads them) and re-derives them on a timer;
// what the operator set (YAML file, per-database fleet fields, API
// overrides) is never derived; a pass that cannot tell what the operator
// set changes nothing.

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) log(component, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, component+": "+fmt.Sprintf(msg, args...))
}

func (l *logCapture) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func selfConfigPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := autonomyPool(t)
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config_derivation")
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config_derived_setting")
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config WHERE key = 'collector.interval_seconds'")
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func newTestRunner(pool *pgxpool.Pool, cfg *config.Config, logs *logCapture) *selfConfigRunner {
	e := selfconfig.NewEngine(selfconfig.NewStore(pool))
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	return &selfConfigRunner{name: "orders", pool: pool, cfg: cfg, engine: e,
		gather: selfconfig.Gather, logInfo: logs.log, logWarn: logs.log}
}

func byKey(results []selfconfig.Result) map[string]selfconfig.Result {
	out := map[string]selfconfig.Result{}
	for _, r := range results {
		out[r.Key] = r
	}
	return out
}

// On a small database every rule's evidence supports the default: nothing
// is shadowed, nothing changes and the ledger stays empty.
func TestSelfConfigOnASmallDatabaseKeepsTheDefaults(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	logs := &logCapture{}
	results, err := newTestRunner(pool, cfg, logs).pass(t.Context(), selfconfig.PhaseStartup)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(selfconfig.Rules()) {
		t.Fatalf("results %+v", results)
	}
	for _, r := range results {
		if r.Status != selfconfig.StatusDefault || r.Applied || r.Shadow != nil {
			t.Errorf("%s on a small database: %+v", r.Key, r)
		}
	}
	if !reflect.DeepEqual(cfg, config.DefaultConfig()) {
		t.Fatal("the runtime config changed")
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM sage.config_derivation").
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("ledger rows %d (%v)", n, err)
	}
}

func TestSelfConfigOperatorSetFromFileAndOverrides(t *testing.T) {
	pool := selfConfigPool(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("sre:\n  detectors:\n    lwlock_waiters: 12\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.NewConfigStore(pool).SetOverride(t.Context(),
		"collector.interval_seconds", "120", 0, 0); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.SRE.Detectors.LWLockWaiters = 12
	cfg.Collector.IntervalSeconds = 120
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	r.configPath, r.controlPool = path, pool
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{MaxConnections: selfconfig.Known(3000),
			CollectorCostMs: selfconfig.Known(5000)}, nil
	}
	results, err := r.pass(t.Context(), selfconfig.PhaseStartup)
	if err != nil {
		t.Fatal(err)
	}
	got := byKey(results)
	if got["sre.detectors.lwlock_waiters"].Status != selfconfig.StatusOperator ||
		got["collector.interval_seconds"].Status != selfconfig.StatusOperator {
		t.Fatalf("operator keys derived: %+v", got)
	}
	if cfg.SRE.Detectors.LWLockWaiters != 12 || cfg.Collector.IntervalSeconds != 120 {
		t.Fatalf("operator values overwritten: %d %d", cfg.SRE.Detectors.LWLockWaiters,
			cfg.Collector.IntervalSeconds)
	}
}

func TestSelfConfigUnreadableConfigFileChangesNothing(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	r.configPath = filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := r.pass(t.Context(), selfconfig.PhaseStartup); err == nil ||
		!errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pass with an unreadable config file: %v", err)
	}
	var n int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM sage.config_derived_setting").Scan(&n); err != nil || n != 0 {
		t.Fatalf("state rows written: %d (%v)", n, err)
	}
}

func TestSelfConfigStartupLogsASummary(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{CatalogScanMs: selfconfig.Known(300)}, nil
	}
	r.deriveAtStartup(t.Context())
	out := logs.text()
	if !strings.Contains(out, `db "orders": derived settings:`) ||
		!strings.Contains(out, "safety.query_timeout_ms=500 (shadow 1200)") {
		t.Fatalf("startup summary:\n%s", out)
	}
}

func TestSelfConfigDisabledDoesNothing(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	cfg.SelfConfig.Enabled = false
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{CatalogScanMs: selfconfig.Known(300)}, nil
	}
	r.deriveAtStartup(t.Context())
	var n int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM sage.config_derived_setting").Scan(&n); err != nil || n != 0 {
		t.Fatalf("disabled self-config wrote %d rows (%v)", n, err)
	}
	if !strings.Contains(logs.text(), "self_config.enabled") {
		t.Fatalf("disabled state not logged:\n%s", logs.text())
	}
}

// Evidence that fails to read is logged; what was read still counts.
func TestSelfConfigPartialEvidence(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{MaxConnections: selfconfig.Known(1000)},
			errors.New("selfconfig evidence catalog scan: canceled")
	}
	results, err := r.pass(t.Context(), selfconfig.PhaseStartup)
	if err != nil {
		t.Fatalf("partial evidence failed the pass: %v", err)
	}
	if got := byKey(results)["sre.detectors.lwlock_waiters"]; got.Shadow == nil ||
		*got.Shadow != 20 {
		t.Fatalf("lwlock from the evidence that was read: %+v", got)
	}
	if !strings.Contains(logs.text(), "catalog scan") {
		t.Fatalf("evidence failure not logged:\n%s", logs.text())
	}
}

// The collector's measured self-cost feeds the collector interval rule;
// the temp rate is measured between passes.
func TestSelfConfigLivePassUsesTheCollectorCost(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	logs := &logCapture{}
	r := newTestRunner(pool, cfg, logs)
	r.cost = func() (float64, bool) { return 1800, true }
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{}, nil
	}
	results, err := r.pass(t.Context(), selfconfig.PhaseLive)
	if err != nil {
		t.Fatal(err)
	}
	if got := byKey(results)["collector.interval_seconds"]; got.Shadow == nil ||
		*got.Shadow != 180 {
		t.Fatalf("collector interval from the measured cost: %+v", got)
	}
	r.cost = func() (float64, bool) { return 0, false }
	results, err = r.pass(t.Context(), selfconfig.PhaseLive)
	if err != nil {
		t.Fatal(err)
	}
	if got := byKey(results)["collector.interval_seconds"]; got.Shadow == nil {
		t.Fatalf("an unknown cost dropped the shadow: %+v", got)
	}
}

func TestSelfConfigRunnerFromRuntime(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ConfigPath = "/etc/pg_sage/config.yaml"
	rt := &databaseRuntime{cfg: cfg, spec: databaseRuntimeSpec{Name: "orders",
		DatabaseID: 7}}
	r := rt.newSelfConfigRunner()
	if r.name != "orders" || r.databaseID != 7 || r.cfg != cfg || r.engine == nil ||
		r.gather == nil || r.configPath != "/etc/pg_sage/config.yaml" {
		t.Fatalf("runner %+v", r)
	}
}
