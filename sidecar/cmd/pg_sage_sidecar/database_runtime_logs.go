package main

import (
	"context"
	"net"
	"strconv"
	"sync"
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

// logFanoutSet shares one log watcher per PostgreSQL cluster (host:port)
// across every runtime: databases on one cluster share its log directory.
type logFanoutSet struct {
	mu      sync.Mutex
	fanouts map[string]*logwatch.LogFanout
}

func newLogFanoutSet() *logFanoutSet {
	return &logFanoutSet{fanouts: map[string]*logwatch.LogFanout{}}
}

// runtimeLogFanouts is the process-wide set every mode uses.
var runtimeLogFanouts = newLogFanoutSet()

// forCluster returns the cluster's fanout, starting its watcher and drain
// loop on first use. ctx is the process lifetime: the watcher outlives any
// one database. nil means log watching is unavailable for the cluster; a
// later database retries.
func (s *logFanoutSet) forCluster(
	ctx context.Context, key string, pool *pgxpool.Pool,
) *logwatch.LogFanout {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fanout := s.fanouts[key]; fanout != nil {
		return fanout
	}
	settings, err := resolvedLogWatchConfig(ctx, pool)
	if err != nil {
		logWarn("logwatch", "[%s] auto-detect: %v", key, err)
		return nil
	}
	watcher := logwatch.NewFileWatcher(settings, logStructuredWrapper)
	if err := watcher.Start(ctx); err != nil {
		logWarn("logwatch", "[%s] start failed: %v", key, err)
		return nil
	}
	fanout := logwatch.NewLogFanout(watcher)
	s.fanouts[key] = fanout
	go runFanoutDrain(ctx, key, fanout, logwatchPollInterval())
	logInfo("logwatch", "[%s] started (dir=%s format=%s)",
		key, settings.LogDirectory, settings.Format)
	warnLogLocale(ctx, key, pool)
	return fanout
}

// logLocaleProbeTimeout bounds the one pg_settings read of lc_messages.
const logLocaleProbeTimeout = 5 * time.Second

// warnLogLocale warns once per cluster when lc_messages is not C or
// English: the text-matched log signals would then never fire. Without a
// pool (an explicitly configured log path needs none) there is nothing
// to read.
func warnLogLocale(ctx context.Context, key string, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, logLocaleProbeTimeout)
	defer cancel()
	setting, err := logwatch.DetectLCMessages(ctx, pool)
	if err != nil {
		logWarn("logwatch", "[%s] %v", key, err)
		return
	}
	if msg := logwatch.LCMessagesWarning(setting); msg != "" {
		logWarn("logwatch", "[%s] %s", key, msg)
	}
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

func logwatchPollInterval() time.Duration {
	interval := time.Duration(cfg.LogWatch.PollIntervalMs) * time.Millisecond
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

// clusterLogFanout returns the shared log fanout when a log consumer (RCA
// or migration log detection) is enabled.
func (rt *databaseRuntime) clusterLogFanout() *logwatch.LogFanout {
	logConsumer := cfg.RCA.Enabled ||
		(cfg.Migration.Enabled && cfg.Migration.LogDetection)
	if !cfg.LogWatch.Enabled || !logConsumer {
		return nil
	}
	conn := rt.spec.Pool.Config().ConnConfig
	key := net.JoinHostPort(conn.Host, strconv.Itoa(int(conn.Port)))
	parent := shutdownCtx
	if parent == nil {
		parent = context.Background()
	}
	return runtimeLogFanouts.forCluster(parent, key, rt.spec.Pool)
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
	if rt.logFanout != nil {
		sub := rt.logFanout.Subscribe(rt.spec.Name)
		rt.rca.SetLogSource(sub)
		critical = sub
	}
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
