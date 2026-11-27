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

// benchIngestInterval is how often the shipped bench reports and
// bench_results_path are re-read.
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
	if !settings.Enforce {
		logWarn(rt.spec.Scope, "db %q: sre.autonomy.enforce is false: self-initiated "+
			"actions (custodians, index create/drop, GUC, vacuum, analyze, hints) run "+
			"under the elapsed-time trust ramp without earned evidence", rt.spec.Name)
	}
	// The M5 SLO engine is built with the investigator, before execution.
	err := processAutonomy().install(rt.ctx, ex, autonomyBinding{database: rt.spec.Name,
		control: control, monitored: rt.spec.Pool, databaseID: rt.recordID(),
		settings: settings, budget: slobudget.New(rt.sloEngine),
		notifier: rt.ledgerNotifier(), runningModel: llmModelSource(rt.generalLLM),
		ha: rt.ha})
	if err != nil {
		logError(rt.spec.Scope, "db %q: trust ledger unavailable; self-initiated "+
			"actions are blocked until it is: %v", rt.spec.Name, err)
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
	notifier := rt.ledgerNotifier()
	rec := earned.NewReconciler(entry.Service, rt.spec.Pool, rt.spec.Name, notifier)
	name := rt.spec.Name
	rt.startShadowScoring()
	rt.start(func() {
		every(rt.ctx, settings.ReconcileInterval(), func(ctx context.Context) {
			if _, err := rec.RunOnce(ctx); err != nil {
				logWarn("autonomy", "db %q: record live outcomes: %v", name, err)
			}
		})
	})
	rt.start(func() {
		every(rt.ctx, settings.EvaluateInterval(), evaluator(entry, notifier))
	})
	rt.start(func() { every(rt.ctx, benchIngestInterval, benchIngester(entry.Service, settings)) })
	rt.startLocalBench(entry.Service, settings)
	rt.startGameDays(entry.Service, settings)
	rt.start(func() {
		<-rt.ctx.Done()
		ledgers.registry.Remove(name)
		ledgers.gameDays.Remove(name)
	})
	rt.note("earned_autonomy")
}

// ledgerNotifier tells this database's operator about ledger events
// through its notification rules (and the log).
func (rt *databaseRuntime) ledgerNotifier() autonomyNotifier {
	n := autonomyNotifier{}
	if rt.dispatcher != nil { // no typed nil in the interface
		n.dispatcher = rt.dispatcher
	}
	return n
}

// evaluator proposes the promotions the evidence supports, then sweeps
// the earned levels whose evidence decays (earned-06): a decay is
// recorded as a cap without waiting for an action, and an expiry a week
// away is warned about once.
func evaluator(entry earned.RegistryEntry, n earned.ExpiryNotifier) func(context.Context) {
	return func(ctx context.Context) {
		created, err := entry.Service.ProposePromotions(ctx)
		if err != nil {
			logWarn("autonomy", "evaluate promotions: %v", err)
		}
		for _, p := range created {
			logInfo("autonomy", "proposed %s/%s %s -> %s: an admin must approve it",
				p.Family, p.Class, p.From, p.To)
		}
		if entry.Limiter == nil {
			return
		}
		res, err := entry.Limiter.Sweep(ctx, n)
		if err != nil {
			logWarn("autonomy", "db %q: sweep decaying evidence: %v",
				entry.Service.Database(), err)
		}
		if res.Capped+res.Cleared+res.Warned > 0 {
			logInfo("autonomy", "db %q: evidence sweep of %d pairs: %d capped, %d cleared, "+
				"%d expiry warnings", entry.Service.Database(), res.Pairs, res.Capped,
				res.Cleared, res.Warned)
		}
	}
}

// startGameDays runs scheduled game days when they are enabled and a
// disposable target exists.
func (rt *databaseRuntime) startGameDays(svc *earned.Service, s config.SREAutonomyConfig) {
	provider, name, err := gameDayProvider(s.GameDays, rt.cfg.Clone,
		localTargetGuardFor(rt.spec.Pool, svc.Store().Pool()))
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
			Interval: time.Duration(s.GameDays.IntervalHours) * time.Hour,
			Warn:     gameDayWarn, CustomerPrograms: s.GameDays.CustomerPrograms,
			Snapshots: rt.tableSnapshots()},
			provider, srebench.GameDayFaults{}, svc, store)
		if err == nil {
			processAutonomy().gameDays.Register(rt.spec.Name, runner)
			delay := gameDayStartDelay(rt.spec.Name, gameDayCheckInterval)
			rt.start(func() {
				everyAfter(rt.ctx, delay, gameDayCheckInterval, gameDayTick(runner))
			})
		}
	}
	if err != nil {
		logError("autonomy", "db %q: game days off: %v", rt.spec.Name, err)
	}
}

// gameDayWarn logs a game day that scored nothing.
func gameDayWarn(format string, args ...any) { logWarn("autonomy", format, args...) }

// tableSnapshots is this database's collector, which a game day's
// customer programs pick their table from (benches-12: bindings are per
// database); nil before the collector exists, so they skip.
func (rt *databaseRuntime) tableSnapshots() gameday.TableSnapshots {
	if rt.collector == nil {
		return nil
	}
	return rt.collector
}

// gameDayTick first abandons game days a stopped process left running
// past gameday.MaxRunTime (destroying their clones), then runs one when due.
func gameDayTick(runner *gameday.Runner) func(context.Context) {
	return func(ctx context.Context) {
		if n, err := runner.AbandonStale(ctx); err != nil {
			logWarn("autonomy", "abandon stale game days: %v", err)
		} else if n > 0 {
			logInfo("autonomy", "abandoned %d stale game day(s) and destroyed their clones", n)
		}
		gd, ran, err := runner.RunDue(ctx)
		if err != nil {
			logWarn("autonomy", "game day %s on %s: %v", gd.ID, gd.Database, err)
		} else if ran {
			logInfo("autonomy", "game day %s on %s: %s", gd.ID, gd.Database, gd.Status)
		}
	}
}

// gameDayProvider is the disposable target of game days: the configured
// clone provider, else the local development database (never on the
// cluster of a database guard refuses), else none (off).
func gameDayProvider(s config.SREGameDaysConfig, cloneCfg config.CloneProviderConfig,
	guard localTargetGuard) (clone.Provider, string, error) {
	if !s.Enabled {
		return nil, "", nil
	}
	if cloneCfg.Provider == "dle" || cloneCfg.Provider == clone.LakebaseAdapter ||
		cloneCfg.Provider == clone.NeonAdapter || cloneCfg.Provider == clone.AuroraAdapter {
		p, err := cloneProviderFor(cloneCfg, guard.own)
		return p, cloneCfg.Provider, err
	}
	if s.LocalDSN != "" {
		p, err := gameday.NewLocalProvider(s.LocalDSN, guard.dsns, guard.pools)
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
