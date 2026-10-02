package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/slobudget"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/gameday"
	srebench "github.com/pg-sage/sidecar/sre-bench"
)

// benchIngestInterval is how often bench_results_path is re-read.
const benchIngestInterval = time.Hour

// gameDayCheckInterval is how often a due game day is looked for.
const gameDayCheckInterval = time.Hour

// installAutonomy binds the database's executor to its earned-autonomy
// ledger before the standing gate is built (sre.autonomy.enforce).
func (rt *databaseRuntime) installAutonomy(ex *executor.Executor) {
	settings := rt.cfg.SRE.Autonomy
	control := rt.spec.ControlPool
	if control == nil {
		control = rt.spec.Pool
	}
	var databaseID *int
	if rt.spec.DatabaseID > 0 {
		id := rt.spec.DatabaseID
		databaseID = &id
	}
	if !settings.Enforce {
		logWarn(rt.spec.Scope, "db %q: sre.autonomy.enforce is false: incident-family "+
			"actions (custodian freeze, WAL bounds) run under the trust ramp without "+
			"earned evidence", rt.spec.Name)
	}
	// The M5 SLO engine is built with the investigator, before execution.
	err := processAutonomy().install(rt.ctx, ex, autonomyBinding{database: rt.spec.Name,
		control: control, monitored: rt.spec.Pool, databaseID: databaseID,
		settings: settings, budget: slobudget.New(rt.sloEngine)})
	if err != nil {
		logError(rt.spec.Scope, "db %q: earned-autonomy ledger unavailable; "+
			"incident-family actions are blocked until it is: %v", rt.spec.Name, err)
	}
}

// startAutonomyLoops keeps the database's ledger current: live outcomes,
// promotion proposals, bench reports and game days.
func (rt *databaseRuntime) startAutonomyLoops() {
	ledgers := processAutonomy()
	entry, ok := ledgers.registry.Lookup(rt.spec.Name)
	if !ok {
		return
	}
	settings := rt.cfg.SRE.Autonomy
	notifier := autonomyNotifier{}
	if rt.dispatcher != nil {
		notifier.dispatcher = rt.dispatcher
	}
	rec := earned.NewReconciler(entry.Service, rt.spec.Pool, rt.spec.Name, notifier)
	name := rt.spec.Name
	rt.start(func() {
		every(rt.ctx, settings.ReconcileInterval(), func(ctx context.Context) {
			if _, err := rec.RunOnce(ctx); err != nil {
				logWarn("autonomy", "db %q: record live outcomes: %v", name, err)
			}
		})
	})
	rt.start(func() { every(rt.ctx, settings.EvaluateInterval(), evaluator(entry.Service)) })
	if path := settings.BenchResultsPath; path != "" {
		rt.start(func() { every(rt.ctx, benchIngestInterval, benchIngester(entry.Service, path)) })
	}
	rt.startGameDays(entry.Service, settings)
	rt.start(func() {
		<-rt.ctx.Done()
		ledgers.registry.Remove(name)
		ledgers.gameDays.Remove(name)
	})
	rt.note("earned_autonomy")
}

func evaluator(svc *earned.Service) func(context.Context) {
	return func(ctx context.Context) {
		created, err := svc.ProposePromotions(ctx)
		if err != nil {
			logWarn("autonomy", "evaluate promotions: %v", err)
			return
		}
		for _, p := range created {
			logInfo("autonomy", "proposed %s/%s %s -> %s: an admin must approve it",
				p.Family, p.Class, p.From, p.To)
		}
	}
}

func benchIngester(svc *earned.Service, path string) func(context.Context) {
	return func(ctx context.Context) {
		n, err := ingestBenchPath(ctx, svc, path)
		if err != nil {
			logWarn("autonomy", "ingest bench reports from %s: %v", path, err)
		}
		if n > 0 {
			logInfo("autonomy", "ingested %d new PGIncidentBench reports", n)
		}
	}
}

// startGameDays runs scheduled game days when they are enabled and a
// disposable target exists.
func (rt *databaseRuntime) startGameDays(svc *earned.Service, s config.SREAutonomyConfig) {
	provider, name, err := gameDayProvider(s.GameDays, rt.cfg.Clone,
		monitoredDSNs(rt.spec.Pool))
	if err != nil {
		logError("autonomy", "db %q: game days off: %v", rt.spec.Name, err)
		return
	}
	if provider == nil {
		return
	}
	store, err := gameday.NewStore(svc.Store().Pool(), svc.Store().DeploymentID())
	if err == nil {
		var runner *gameday.Runner
		runner, err = gameday.NewRunner(gameday.Config{Database: rt.spec.Name, Provider: name,
			Families: trimmedFamilies(s.GameDays.Families),
			Interval: time.Duration(s.GameDays.IntervalHours) * time.Hour},
			provider, srebench.GameDayFaults{}, svc, store)
		if err == nil {
			processAutonomy().gameDays.Register(rt.spec.Name, runner)
			rt.start(func() { every(rt.ctx, gameDayCheckInterval, gameDayTick(runner)) })
		}
	}
	if err != nil {
		logError("autonomy", "db %q: game days off: %v", rt.spec.Name, err)
	}
}

func gameDayTick(runner *gameday.Runner) func(context.Context) {
	return func(ctx context.Context) {
		gd, ran, err := runner.RunDue(ctx)
		if err != nil {
			logWarn("autonomy", "game day %s on %s: %v", gd.ID, gd.Database, err)
		} else if ran {
			logInfo("autonomy", "game day %s on %s: %s", gd.ID, gd.Database, gd.Status)
		}
	}
}

// gameDayProvider is the disposable target of game days: the configured
// clone provider, else the local development database, else none (off).
func gameDayProvider(s config.SREGameDaysConfig, cloneCfg config.CloneProviderConfig,
	monitored []string) (clone.Provider, string, error) {
	if !s.Enabled {
		return nil, "", nil
	}
	if cloneCfg.Provider == "dle" || cloneCfg.Provider == "snapshot" {
		p, err := configuredMCPCloneProvider(cloneCfg)
		return p, cloneCfg.Provider, err
	}
	if s.LocalDSN != "" {
		p, err := gameday.NewLocalProvider(s.LocalDSN, monitored)
		if err != nil {
			return nil, "", err
		}
		return p, "local", nil
	}
	logWarn("autonomy", "sre.autonomy.game_days.enabled needs clone.provider or "+
		"game_days.local_dsn; game days are off")
	return nil, "", nil
}

// every runs fn now and then every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	fn(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx)
		}
	}
}
