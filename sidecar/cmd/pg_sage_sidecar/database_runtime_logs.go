package main

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	return fanout
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
	if rt.logFanout != nil {
		rt.rca.SetLogSource(rt.logFanout.Subscribe(rt.spec.Name))
	}
	rt.analyzer.WithRCAEngine(newRCAAdapter(rt.ctx, rt.rca, rt.spec.Pool,
		rt.spec.Name, logStructuredWrapper))
	rt.note("rca")
}

func (rt *databaseRuntime) startSchemaLint() {
	if !cfg.SchemaLint.Enabled {
		return
	}
	runner := lint.NewRunner(
		rt.spec.Pool, &cfg.SchemaLint, rt.pgVersion(), rt.spec.Name,
		logStructuredWrapper,
	)
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
	)
	findings := store.NewMigrationSafetyFindingStore(rt.spec.Pool)
	if cfg.Migration.ActivityPolling {
		detector := migration.NewDetector(
			rt.spec.Pool, advisor, &cfg.Migration, logStructuredWrapper,
		).WithFindingSink(findings)
		rt.start(func() { detector.Run(rt.ctx) })
	}
	rt.note("migration")
	if !cfg.Migration.LogDetection {
		return
	}
	if rt.logFanout == nil {
		logWarn(rt.spec.Scope, "db %q: migration log detection unavailable: "+
			"enable a working logwatch source", rt.spec.Name)
		return
	}
	entries := rt.logFanout.SubscribeEntries(
		"migration:"+rt.spec.Name, rt.spec.Config.Database,
	)
	logDetector := migration.NewLogDetector(
		advisor, logStructuredWrapper,
	).WithFindingSink(findings)
	rt.start(func() { logDetector.Run(rt.ctx, entries, logwatchPollInterval()) })
}
