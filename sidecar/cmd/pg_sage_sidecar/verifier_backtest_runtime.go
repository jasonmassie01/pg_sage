package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// startVerifierBacktest gives the executor the database's verifier
// back-tests (verify.method auto acts on v2 once the latest one passes)
// and runs the back-test weekly on the leader (lab-02), skipped while the
// host guard withholds heavy work.
func (rt *databaseRuntime) startVerifierBacktest() {
	rt.executor.WithBacktestReader(backtest.NewReader(rt.spec.Pool))
	w := weeklyBacktest(rt.cfg, rt.spec.Name, rt.spec.Pool, rt.executor.HostWithhold)
	rt.start(func() { w.Loop(rt.ctx) })
	rt.note("verifier_backtest")
}

// weeklyBacktest is the weekly back-test of database name in pool.
func weeklyBacktest(cfg *config.Config, name string, pool *pgxpool.Pool,
	withhold func(context.Context) []string) *backtest.Weekly {
	run := backtestConfig(cfg, name, backtest.DefaultDays, cfg.Verify.BacktestPlacebos, 1,
		nil)
	return &backtest.Weekly{Config: run,
		Leader:   func() bool { return fleetLeader == nil || fleetLeader.IsLeader() },
		Withhold: withhold,
		Latest: func(ctx context.Context) (backtest.Result, bool, error) {
			return backtest.Latest(ctx, pool)
		},
		Run: func(ctx context.Context, c backtest.Config) (backtest.Result, error) {
			return backtest.RunAndSave(ctx, pool, backtest.NewPostgresHistory(pool), c)
		},
		Log: logInfo}
}
