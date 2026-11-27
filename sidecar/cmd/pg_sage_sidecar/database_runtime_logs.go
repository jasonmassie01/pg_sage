package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/schema/lint"
	"github.com/pg-sage/sidecar/internal/store"
)

// logLocaleProbeTimeout bounds each pg_settings read made when a log
// watcher starts (lc_messages and the signal settings).
const logLocaleProbeTimeout = 5 * time.Second

// logSettingHints warns once per cluster when lc_messages is not C or
// English (the text-matched signals would never fire) or a setting keeps an
// optional signal silent, and returns the warnings for the source status.
// Without a pool (an explicitly configured log path needs none) there is
// nothing to read.
func logSettingHints(ctx context.Context, key string, pool *pgxpool.Pool) []string {
	if pool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, logLocaleProbeTimeout)
	defer cancel()
	var hints []string
	setting, err := logwatch.DetectLCMessages(ctx, pool)
	if err != nil {
		logWarn("logwatch", "[%s] %v", key, err)
	} else if msg := logwatch.LCMessagesWarning(setting); msg != "" {
		hints = append(hints, msg)
	}
	signal, err := logwatch.DetectSignalHints(ctx, pool, cfg.LogWatch.SlowQueryEnabled)
	if err != nil {
		logWarn("logwatch", "[%s] %v", key, err)
	}
	hints = append(hints, signal...)
	for _, h := range hints {
		logWarn("logwatch", "[%s] %s", key, h)
	}
	return hints
}

// resolvedLogWatchConfig fills an unset log directory or format from the
// server's own settings.
func resolvedLogWatchConfig(
	ctx context.Context, pool *pgxpool.Pool,
) (config.LogWatchConfig, error) {
	settings := cfg.LogWatch
	if settings.LogDirectory != "" && settings.Format != "" {
		return settings, nil
	}
	if pool == nil {
		return config.LogWatchConfig{}, errors.New("no running database of this " +
			"cluster to read log_destination and log_directory from pg_settings; " +
			"set logwatch.log_directory and logwatch.format")
	}
	dir, format, err := logwatch.DetectLogSettings(ctx, pool)
	if err != nil {
		return config.LogWatchConfig{}, err
	}
	if settings.LogDirectory == "" {
		settings.LogDirectory = dir
	}
	if settings.Format == "" {
		settings.Format = format
	}
	return settings, nil
}

// logwatchPollInterval reads logwatch.poll_interval_ms under the hot-reload
// lock a dashboard edit writes it under (env-race).
func logwatchPollInterval() time.Duration {
	config.RLockForHotReload()
	interval := time.Duration(cfg.LogWatch.PollIntervalMs) * time.Millisecond
	config.RUnlockForHotReload()
	if interval <= 0 {
		return time.Second
	}
	return interval
}

// runFanoutDrain distributes the cluster's log signals to every subscriber
// until ctx ends, then stops the watcher.
func runFanoutDrain(
	ctx context.Context, key string, fanout *logwatch.LogFanout,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fanout.Stop()
			logInfo("logwatch", "[%s] stopped", key)
			return
		case <-ticker.C:
			fanout.DrainSource()
		}
	}
}

// logConsumerEnabled reports whether anything reads the server log: RCA
// or migration log detection.
func logConsumerEnabled() bool {
	return cfg.RCA.Enabled || (cfg.Migration.Enabled && cfg.Migration.LogDetection)
}

// logWatchWanted reports whether a cluster log source is wired: logwatch
// is not off (auto probes for a readable source, rca-09b) and something
// reads the log.
func logWatchWanted() bool {
	return cfg.LogWatch.Mode() != config.LogWatchOff && logConsumerEnabled()
}

// clusterLogFanout returns the shared log fanout when a log consumer (RCA
// or migration log detection) is enabled.
func (rt *databaseRuntime) clusterLogFanout() *logwatch.LogFanout {
	if !logWatchWanted() {
		return nil
	}
	key := poolHostPort(rt.spec.Pool)
	parent := shutdownCtx
	if parent == nil {
		parent = context.Background()
	}
	fanout := runtimeLogFanouts.forCluster(parent, key, rt.spec.Pool)
	pool := rt.spec.Pool
	rt.start(func() {
		<-rt.ctx.Done()
		runtimeLogFanouts.release(key, pool)
	})
	return fanout
}

// wireRCA attaches the incident engine, fed by the cluster's log watcher.
func (rt *databaseRuntime) wireRCA() {
	rt.logFanout = rt.clusterLogFanout()
	if !cfg.RCA.Enabled {
		return
	}
	rt.rca = rca.NewEngine(&cfg.RCA, logStructuredWrapper)
	if rt.llmOn {
		rt.rca.WithLLM(rt.generalLLM)
	}
	var critical rca.CriticalLogSource
	var logSub rca.LogSource
	if rt.logFanout != nil {
		sub := rt.logFanout.Subscribe(rt.spec.Name)
		rt.rca.SetLogSource(sub)
		critical, logSub = sub, sub
	}
	rt.joinClusterScope(logSub)
	rt.lockObs = analyzer.NewLockChainObserver(rt.cfg.RCA.LockChainInterval())
	rt.rcaAdapter = newRCAAdapter(rcaAdapterDeps{
		ctx: rt.ctx, eng: rt.rca, pool: rt.spec.Pool, name: rt.spec.Name,
		cfg: rt.cfg, logFn: logStructuredWrapper, workers: rt.workers,
		probes: rt.probes, logSource: critical, lockObs: rt.lockObs,
	})
	rt.analyzer.WithRCAEngine(rt.rcaAdapter)
	rt.analyzer.WithLockChainObserver(rt.lockObs)
	rt.note("rca")
	if rt.rcaAdapter.fastPath != nil {
		rt.note("sre_fast_path")
	}
	if rt.rcaAdapter.logFastPath != nil {
		rt.note("log_fast_path")
	}
}

func (rt *databaseRuntime) startSchemaLint() {
	if !cfg.SchemaLint.Enabled {
		return
	}
	runner := lint.NewRunner(
		rt.spec.Pool, &cfg.SchemaLint, rt.pgVersion(), rt.spec.Name,
		logStructuredWrapper,
	)
	runner.SetCatalogReadTimeouts(catalogread.FromSafety(cfg.Safety))
	if rt.llmOn {
		runner.SetLLMClient(rt.generalLLM)
	}
	rt.start(func() { runner.Run(rt.ctx) })
	rt.note("lint")
}

// startMigrationAdvisor runs the DDL safety advisor from activity polling
// and, with a log watcher, from this database's own log entries.
func (rt *databaseRuntime) startMigrationAdvisor() {
	if !cfg.Migration.Enabled {
		return
	}
	var general *llm.Client
	if rt.llmManager != nil {
		general = rt.llmManager.General
	}
	advisor := migration.NewAdvisor(
		rt.spec.Pool, &cfg.Migration, rt.pgVersion(), rt.spec.Name,
		logStructuredWrapper, general,
	).WithCatalogReadTimeouts(catalogread.FromSafety(cfg.Safety))
	logs := cfg.Migration.LogDetection
	if logs && rt.logFanout == nil {
		logWarn(rt.spec.Scope, "db %q: migration log detection unavailable: "+
			"enable a working logwatch source", rt.spec.Name)
		logs = false
	}
	detector, logDetector := newMigrationDetectors(rt.spec.Pool, advisor,
		store.NewMigrationSafetyFindingStore(rt.spec.Pool), &cfg.Migration, logs)
	// One model worker per database, off both detectors' paths
	// (migration-10); it returns at once without a model.
	rt.start(func() { advisor.RunEnrichment(rt.ctx) })
	if detector != nil {
		if rt.lockObs != nil {
			detector.WithLockQueue(rt.lockObs)
			rt.lockObs.SetQueuedDDL(detector)
		}
		rt.start(func() { detector.Run(rt.ctx) })
	}
	rt.note("migration")
	if logDetector == nil {
		return
	}
	entries := rt.logFanout.SubscribeEntries(
		"migration:"+rt.spec.Name, rt.spec.Config.Database,
	)
	rt.start(func() { logDetector.Run(rt.ctx, entries, logwatchPollInterval()) })
}

// newMigrationStaleResolver builds a database's resolver of stale
// migration findings (a variable so tests can count constructions).
var newMigrationStaleResolver = migration.NewStaleResolver

// newMigrationDetectors builds one database's activity detector (when
// mcfg.ActivityPolling) and log detector (when logs). Both share one
// stale-finding resolver, so the resolve UPDATE runs once per interval
// rather than once per detector (migration-12).
func newMigrationDetectors(
	pool *pgxpool.Pool, advisor *migration.Advisor,
	findings *store.MigrationSafetyFindingStore, mcfg *config.MigrationConfig, logs bool,
) (*migration.Detector, *migration.LogDetector) {
	activity := mcfg.ActivityPolling
	if !activity && !logs {
		return nil, nil
	}
	stale := newMigrationStaleResolver(pool, logStructuredWrapper)
	var detector *migration.Detector
	if activity {
		detector = migration.NewDetector(pool, advisor, mcfg,
			logStructuredWrapper).WithFindingSink(findings).WithStaleResolver(stale)
	}
	var logDetector *migration.LogDetector
	if logs {
		logDetector = migration.NewLogDetector(advisor, logStructuredWrapper).
			WithFindingSink(findings).WithStaleResolver(stale)
	}
	return detector, logDetector
}
