package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/backtestfx"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// pg_sage_sidecar verifier-backtest (lab-02): flags refused by name, a
// database the configuration does not register refused, the report, and
// on a real server one stored row whose counts match the in-memory run.

func TestBacktestCmd_RefusesBadFlagsByName(t *testing.T) {
	cases := map[string][]string{
		"--days":      {"--database", "db", "--days", "0"},
		"--placebos":  {"--database", "db", "--placebos", "0"},
		"--database":  {"--days", "30"},
		"unexpected":  {"--database", "db", "stray"},
		"-placebos":   {"--database", "db", "--placebos", "many"},
		"-days value": {"--database", "db", "--days", "-1"},
	}
	for want, args := range cases {
		_, err := parseBacktestArgs(args)
		name := strings.TrimSuffix(want, " value")
		if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(name, "-")) {
			t.Errorf("%v: %v, want an error naming %s", args, err, name)
		}
	}
	a, err := parseBacktestArgs([]string{"--database", "db", "--seed", "9", "--json"})
	if err != nil || a.days != 90 || a.placebos != 300 || a.seed != 9 || !a.asJSON {
		t.Fatalf("defaults: %+v %v", a, err)
	}
}

func fleetConfig(names ...string) *config.Config {
	cfg := config.DefaultConfig()
	for _, n := range names {
		cfg.Databases = append(cfg.Databases, config.DatabaseConfig{Name: n})
	}
	return cfg
}

func TestBacktestCmd_RefusesAnUnregisteredDatabase(t *testing.T) {
	opened := false
	deps := backtestDeps{
		load: func(string) (*config.Config, error) { return fleetConfig("orders", "app"), nil },
		open: func(context.Context, string) (*pgxpool.Pool, error) {
			opened = true
			return nil, errors.New("must not connect")
		},
		now: time.Now}
	var out, errOut bytes.Buffer
	code := runVerifierBacktest(context.Background(), []string{"--database", "ledger"}, deps,
		&out, &errOut)
	want := `--database "ledger" is not a registered database (registered: app, orders)`
	if code != 2 || !strings.Contains(errOut.String(), want) || opened || out.Len() != 0 {
		t.Fatalf("exit %d, stderr %q, opened %v; want 2 and %q", code, errOut.String(),
			opened, want)
	}
	deps.load = func(string) (*config.Config, error) { return nil, errors.New("bad yaml") }
	if code := runVerifierBacktest(context.Background(), []string{"--database", "orders"},
		deps, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "bad yaml") {
		t.Fatalf("load failure: exit %d, %q", code, errOut.String())
	}
}

func TestBacktestCmd_PrintsTheInconclusiveShare(t *testing.T) {
	r := backtest.Result{Database: "db", Verifier: "v2", V2: backtest.Counts{Placebos: 300,
		Inconclusive: 300, Injected: 300}}
	var text bytes.Buffer
	if err := printBacktest(&text, r, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"inconclusive share 100%",
		"gate: refused: detection 0% below 80%", "verify.method auto stays in shadow"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	var js bytes.Buffer
	if err := printBacktest(&js, r, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Passed bool            `json:"gate_passed"`
		Reason string          `json:"gate_reason"`
		V2     backtest.Counts `json:"v2"`
	}
	if err := json.Unmarshal(js.Bytes(), &got); err != nil || got.Passed ||
		got.Reason != "detection 0% below 80%" || got.V2 != r.V2 {
		t.Fatalf("json = %+v %v:\n%s", got, err, js.String())
	}
}

func TestBacktestCmd_ConfigFromSettings(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Verify.BacktestMaxRuntimeMinutes, cfg.Verify.BacktestPlacebos = 7, 450
	cfg.Collector.IntervalSeconds = 30
	c := backtestConfig(cfg, "db", 60, 250, 4, nil)
	if c.Database != "db" || c.Days != 60 || c.Placebos != 250 || c.Seed != 4 ||
		c.MaxRuntime != 7*time.Minute || c.Cycle != 30*time.Second {
		t.Fatalf("backtestConfig = %+v", c)
	}
	w := weeklyBacktest(cfg, "db", nil, nil)
	if w.Config.Placebos != 450 || w.Config.Days != backtest.DefaultDays ||
		w.Config.MaxRuntime != 7*time.Minute || w.Leader == nil || !w.Leader() {
		t.Fatalf("weekly config = %+v", w.Config)
	}
}

func backtestCmdPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, table := range []string{"snapshots", "query_rollup_hourly",
		"query_rollup_coverage", "verifier_backtest"} {
		if _, err := pool.Exec(t.Context(), "DELETE FROM sage."+table); err != nil {
			t.Fatalf("clear sage.%s: %v", table, err)
		}
	}
	return pool, dsn
}

// fxSource is m as backtestfx writes it.
func fxSource(m *backtest.Memory) backtestfx.Source {
	first, last, _, _ := m.Span(context.Background())
	src := backtestfx.Source{First: first, Last: last, Breaks: m.Breaks(),
		Cycles: map[int64][]backtestfx.Cycle{}}
	for _, s := range m.Stmts {
		src.Statements = append(src.Statements, backtestfx.Statement{QueryID: s.QueryID,
			Tables: s.Tables})
		for _, iv := range m.Intervals(s.QueryID) {
			src.Cycles[s.QueryID] = append(src.Cycles[s.QueryID], backtestfx.Cycle{To: iv.To,
				Calls: iv.Calls, Blocks: iv.Blocks, Rows: iv.Rows, Ms: iv.Ms})
		}
	}
	return src
}

func cmdDeps(dsn string, now time.Time) backtestDeps {
	return backtestDeps{
		load: func(string) (*config.Config, error) { return fleetConfig("fixture"), nil },
		open: func(ctx context.Context, _ string) (*pgxpool.Pool, error) {
			return pgxpool.New(ctx, dsn)
		},
		now: func() time.Time { return now }}
}

// Integration: 30 days of synthetic snapshots; the CLI stores one row
// whose counts match the in-memory run, and a run without history stores
// none.
func TestBacktestCmd_StoresTheInMemoryCounts(t *testing.T) {
	pool, dsn := backtestCmdPool(t)
	start := time.Now().UTC().Truncate(time.Hour).Add(-31 * 24 * time.Hour)
	now := start.Add(30 * 24 * time.Hour)
	var out, errOut bytes.Buffer
	args := []string{"--database", "fixture", "--placebos", "24", "--seed", "3"}
	if code := runVerifierBacktest(t.Context(), args, cmdDeps(dsn, now), &out,
		&errOut); code != 1 || !strings.Contains(errOut.String(),
		"not enough history: 0 of 28 days") {
		t.Fatalf("empty history: exit %d, %q", code, errOut.String())
	}
	m, err := backtest.FixtureAt(backtest.FixtureNoisyNeighbour, start, 30, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backtestfx.Persist(t, pool, fxSource(m), 15*time.Minute, 35)
	cfg := backtestConfig(fleetConfig("fixture"), "fixture", 90, 24, 3,
		func() time.Time { return now })
	want, err := backtest.Run(t.Context(), m, cfg)
	if err != nil {
		t.Fatalf("in-memory run: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := runVerifierBacktest(t.Context(), args, cmdDeps(dsn, now), &out,
		&errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.verifier_backtest`).
		Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d, %v; want 1", rows, err)
	}
	got, ok, err := backtest.Latest(t.Context(), pool)
	if err != nil || !ok || got.V2 != want.V2 || got.V1 != want.V1 || got.Seed != 3 {
		t.Fatalf("stored %+v, want the in-memory %+v (%v)", got, want, err)
	}
	if !strings.Contains(out.String(), "verifier back-test of fixture") {
		t.Fatalf("report:\n%s", out.String())
	}
}

// With history.store: meta the command refuses before connecting: it reads
// only the monitored database's history, which is not where it lives.
func TestBacktestCmd_RefusesMetaHistory(t *testing.T) {
	opened := false
	deps := backtestDeps{
		load: func(string) (*config.Config, error) {
			cfg := fleetConfig("orders")
			cfg.History.Store = config.HistoryStoreMeta
			return cfg, nil
		},
		open: func(context.Context, string) (*pgxpool.Pool, error) {
			opened = true
			return nil, errors.New("must not connect")
		},
		now: time.Now}
	var out, errOut bytes.Buffer
	code := runVerifierBacktest(context.Background(), []string{"--database", "orders"}, deps,
		&out, &errOut)
	if code != 2 || opened || out.Len() != 0 ||
		!strings.Contains(errOut.String(), "history.store is meta") ||
		!strings.Contains(errOut.String(), `back-tests "orders" from the meta database`) {
		t.Fatalf("exit %d, stderr %q, opened %v; want 2 refusing meta history", code,
			errOut.String(), opened)
	}
}
