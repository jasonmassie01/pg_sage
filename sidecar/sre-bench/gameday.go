package srebench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// GameDayOptions selects the fault programs of a game day.
type GameDayOptions struct {
	// Families limits the run to these families; empty runs every family.
	Families []string
	// Scenarios limits the run to these scenario ids; empty runs all.
	Scenarios []string
}

// OpenEnv bootstraps the sage schema on dsn's database and returns the
// environment and its close function (for callers without a testing.T).
func OpenEnv(ctx context.Context, dsn string) (*Env, func(), error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect game-day database: %w", err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("bootstrap game-day database: %w", err)
	}
	st, err := sre.NewPostgresStore(pool, benchLimits())
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("game-day store: %w", err)
	}
	e := &Env{DSN: dsn, Pool: pool, Store: st,
		Runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))}
	return e, func() { e.closeSessions(); pool.Close() }, nil
}

// selectGameDayScenarios picks the scenarios of opts; an unknown family
// or scenario id, or an empty selection, is an error.
func selectGameDayScenarios(opts GameDayOptions) ([]Scenario, error) {
	all := Scenarios()
	families := map[string]bool{}
	ids := map[string]bool{}
	for _, sc := range all {
		families[string(sc.Family)], ids[sc.ID] = true, true
	}
	for _, f := range opts.Families {
		if !families[f] {
			return nil, fmt.Errorf("game day: no fault programs for family %q", f)
		}
	}
	for _, id := range opts.Scenarios {
		if !ids[id] {
			return nil, fmt.Errorf("game day: unknown scenario %q", id)
		}
	}
	var out []Scenario
	for _, sc := range all {
		if (len(opts.Families) == 0 || slices.Contains(opts.Families, string(sc.Family))) &&
			(len(opts.Scenarios) == 0 || slices.Contains(opts.Scenarios, sc.ID)) {
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("game day: the selection has no scenario")
	}
	return out, nil
}

// RunGameDay runs the selected fault programs once against the
// disposable database dsn with the deterministic investigator (no model
// tokens are spent on clones) and returns the PGIncidentBench JSON
// report. Never point it at a monitored database: the fault programs
// create blocking sessions, WAL and replication slots.
func RunGameDay(ctx context.Context, dsn string, opts GameDayOptions,
	now time.Time) ([]byte, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("game day: no database")
	}
	scenarios, err := selectGameDayScenarios(opts)
	if err != nil {
		return nil, err
	}
	env, closeEnv, err := OpenEnv(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer closeEnv()
	var version string
	if err := env.Pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		return nil, fmt.Errorf("game day: server version: %w", err)
	}
	cfg := RunConfig{Repeats: 1, Live: []LiveArm{CausalGraph{}}}
	results := Run(ctx, env, scenarios, cfg)
	report := BuildReport(results, ReportMeta{Arms: cfg.ArmNames(), Gated: cfg.Gated(),
		Pending: cfg.Pending(), Repeats: 1, ServerVersion: version,
		GeneratedAt: now.UTC()})
	return json.Marshal(report)
}

// GameDayFaults adapts RunGameDay to the game-day runner.
type GameDayFaults struct {
	// Scenarios limits every run to these ids (empty: all of the families).
	Scenarios []string
	// Now stamps the report (default time.Now).
	Now func() time.Time
}

// Run runs the fault programs of families against dsn.
func (f GameDayFaults) Run(ctx context.Context, dsn string, families []string) ([]byte,
	error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	return RunGameDay(ctx, dsn, GameDayOptions{Families: families, Scenarios: f.Scenarios},
		now())
}
