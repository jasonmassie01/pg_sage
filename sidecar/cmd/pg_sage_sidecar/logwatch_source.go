package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// The retry of an unavailable log source backs off from logRetryMin,
// doubling, to logRetryMax (rca-09).
const (
	logRetryMin = 30 * time.Second
	logRetryMax = 10 * time.Minute
)

// nextLogRetry is the wait after prev: logRetryMin first, then doubled,
// capped at logRetryMax.
func nextLogRetry(prev time.Duration) time.Duration {
	if prev <= 0 {
		return logRetryMin
	}
	return min(prev*2, logRetryMax)
}

// clusterLog is one cluster's shared log source. The fanout exists from
// the first database on, so subscribers taken while the source is
// unavailable receive its signals once a retry starts the watcher.
type clusterLog struct {
	fanout    *logwatch.LogFanout
	watcher   *logwatch.FileWatcher
	pools     []*pgxpool.Pool // live databases' pools, latest last, for re-detection
	status    logwatch.SourceStatus
	active    bool
	retryDone chan struct{} // closed when the retry goroutine ends
	// autoNoted: auto mode has logged this cluster's missing source once.
	autoNoted bool
}

// logFanoutSet shares one log watcher per PostgreSQL cluster (host:port)
// across every runtime: databases on one cluster share its log directory.
type logFanoutSet struct {
	mu       sync.Mutex
	clusters map[string]*clusterLog
	// after is the retry clock; tests replace it.
	after func(time.Duration) <-chan time.Time
	// resolve detects the log settings from pg_settings; tests replace it.
	resolve logSettingsResolver
}

// logSettingsResolver fills the log directory and format of a cluster.
type logSettingsResolver func(context.Context, *pgxpool.Pool) (config.LogWatchConfig, error)

func newLogFanoutSet() *logFanoutSet {
	return &logFanoutSet{clusters: map[string]*clusterLog{}, after: time.After,
		resolve: resolvedLogWatchConfig}
}

// runtimeLogFanouts is the process-wide set every mode uses.
var runtimeLogFanouts = newLogFanoutSet()

// forCluster returns the cluster's fanout. The first call starts its
// watcher and drain loop; when the log cannot be read yet the source is
// unavailable, with the reason, and a per-cluster goroutine retries with
// backoff until it starts or ctx (the process lifetime) ends.
func (s *logFanoutSet) forCluster(
	ctx context.Context, key string, pool *pgxpool.Pool,
) *logwatch.LogFanout {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cl := s.clusters[key]; cl != nil {
		cl.addPool(pool)
		return cl.fanout
	}
	watcher := logwatch.NewFileWatcher(cfg.LogWatch, logStructuredWrapper).
		WithDeadlockStatementText(cfg.RCA.DeadlockStatementText)
	cl := &clusterLog{watcher: watcher, retryDone: make(chan struct{}),
		fanout: logwatch.NewLogFanout(watcher).WithClusterKey(key)}
	cl.addPool(pool)
	s.clusters[key] = cl
	// The first attempt holds s.mu so every database of the cluster sees
	// its outcome; retries run outside it.
	if s.record(ctx, key, cl, s.attempt(ctx, key, cl, pool)) {
		close(cl.retryDone)
		return cl.fanout
	}
	go s.retry(ctx, key, cl)
	return cl.fanout
}

// logStartTimeout bounds the pg_settings detection of one start attempt.
const logStartTimeout = 10 * time.Second

// attempt runs one start and turns a failure into the status of the
// logwatch mode: explicit true is unavailable with a WARN; auto is
// disabled with the detector's reason, noted by one INFO line until the
// source starts (rca-09b). Only the cluster's first call or its single
// retry goroutine calls it, never both at once, and before the watcher's
// drain loop exists.
func (s *logFanoutSet) attempt(
	ctx context.Context, key string, cl *clusterLog, pool *pgxpool.Pool,
) logwatch.SourceStatus {
	st, err := startClusterLog(ctx, key, cl.watcher, pool, s.resolve)
	if err == nil {
		return st
	}
	if cfg.LogWatch.Mode() != config.LogWatchAuto {
		logWarn("logwatch", "[%s] log source unavailable: %v", key, err)
		return logwatch.UnavailableStatus(err.Error())
	}
	if !cl.autoNoted {
		cl.autoNoted = true
		logInfo("logwatch", "[%s] log RCA waits for a readable log source "+
			"(logwatch.enabled: auto): %v", key, err)
	}
	st = logwatch.DisabledStatus("auto: " + err.Error())
	st.Hints = []string{autoLogHint}
	return st
}

// autoLogHint tells an operator how an auto-disabled log source starts.
const autoLogHint = "logwatch.enabled is auto: log RCA starts by itself once the " +
	"server writes a readable jsonlog (PostgreSQL 15+) or csvlog file; set " +
	"logwatch.enabled: false to stop probing"

// startClusterLog detects the log source and starts the watcher once.
func startClusterLog(
	ctx context.Context, key string, watcher *logwatch.FileWatcher, pool *pgxpool.Pool,
	resolve logSettingsResolver,
) (logwatch.SourceStatus, error) {
	detectCtx, cancel := context.WithTimeout(ctx, logStartTimeout)
	defer cancel()
	settings, err := resolve(detectCtx, pool)
	if err != nil {
		return logwatch.SourceStatus{}, fmt.Errorf("auto-detect: %w", err)
	}
	if err := watcher.StartWithConfig(ctx, settings); err != nil {
		return logwatch.SourceStatus{}, fmt.Errorf("cannot read logwatch.log_directory "+
			"%s (%w): mount the server's log_directory read-only into the sidecar and "+
			"check that logging_collector = on and log_destination includes %s",
			settings.LogDirectory, err, settings.Format)
	}
	logInfo("logwatch", "[%s] started (dir=%s format=%s)",
		key, settings.LogDirectory, settings.Format)
	return logwatch.SourceStatus{State: logwatch.SourceActive, Format: settings.Format,
		Hints: logSettingHints(ctx, key, pool)}, nil
}

// record stores an attempt's outcome (s.mu held) and, when the source is
// active, starts the drain loop.
func (s *logFanoutSet) record(
	ctx context.Context, key string, cl *clusterLog, st logwatch.SourceStatus,
) bool {
	cl.status = st
	if st.State != logwatch.SourceActive {
		return false
	}
	cl.active = true
	go runFanoutDrain(ctx, key, cl.fanout, logwatchPollInterval())
	return true
}

// retry re-attempts an unavailable source with backoff and ends when it
// starts or ctx ends.
func (s *logFanoutSet) retry(ctx context.Context, key string, cl *clusterLog) {
	defer close(cl.retryDone)
	wait := time.Duration(0)
	for {
		wait = nextLogRetry(wait)
		s.mu.Lock()
		next := time.Now().Add(wait)
		cl.status.NextRetryAt = &next
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-s.after(wait):
		}
		s.mu.Lock()
		pool := cl.latestPool()
		s.mu.Unlock()
		st := s.attempt(ctx, key, cl, pool)
		s.mu.Lock()
		started := s.record(ctx, key, cl, st)
		s.mu.Unlock()
		if started {
			return
		}
	}
}

func (cl *clusterLog) addPool(pool *pgxpool.Pool) {
	if pool != nil {
		cl.pools = append(cl.pools, pool)
	}
}

// latestPool is the pool of the newest database still running, or nil.
func (cl *clusterLog) latestPool() *pgxpool.Pool {
	if n := len(cl.pools); n > 0 {
		return cl.pools[n-1]
	}
	return nil
}

// release forgets a removed database's pool, so a retry never detects
// through a closed pool while another database of the cluster runs.
func (s *logFanoutSet) release(key string, pool *pgxpool.Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cl := s.clusters[key]
	if cl == nil || pool == nil {
		return
	}
	if i := slices.Index(cl.pools, pool); i >= 0 {
		cl.pools = slices.Delete(cl.pools, i, i+1)
	}
}

// status is the cluster's log-source status with the watcher's live
// progress, or a zero value for a cluster never wired.
func (s *logFanoutSet) status(key string) logwatch.SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	cl := s.clusters[key]
	if cl == nil {
		return logwatch.SourceStatus{}
	}
	st := cl.status
	if cl.active {
		live := cl.watcher.Status()
		st.File, st.LastLineAt = live.File, live.LastLineAt
		st.ParseErrors, st.DroppedLines = live.ParseErrors, live.DroppedLines
	}
	st.Hints = append([]string(nil), st.Hints...)
	return st
}

// retryDone is closed once the cluster's retry has ended (or was never
// needed); an unknown cluster returns a closed channel.
func (s *logFanoutSet) retryDone(key string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cl := s.clusters[key]; cl != nil {
		return cl.retryDone
	}
	done := make(chan struct{})
	close(done)
	return done
}

// providerLogFormat is the format a provider log API reports: RCA reads
// Supabase logs through its Management API, not the cluster's files.
const providerLogFormat = "supabase_api"

// logSourceStatusFor is a database's log-source status: disabled with the
// setting that turns it off, active when RCA reads the provider's log API,
// else its cluster's status.
func logSourceStatusFor(
	set *logFanoutSet, key string, providerLogs bool,
) logwatch.SourceStatus {
	if cfg.LogWatch.Mode() == config.LogWatchOff {
		return logwatch.DisabledStatus("logwatch.enabled is false")
	}
	if !logConsumerEnabled() {
		return logwatch.DisabledStatus("no log consumer: rca.enabled is false and " +
			"migration log detection is off")
	}
	if providerLogs {
		return logwatch.SourceStatus{State: logwatch.SourceActive, Format: providerLogFormat}
	}
	st := set.status(key)
	if st.State == "" {
		return logwatch.UnavailableStatus("log watcher not started for this database")
	}
	return st
}

// logSourceStatus is this runtime's log-source status for the fleet API.
func (rt *databaseRuntime) logSourceStatus() logwatch.SourceStatus {
	return logSourceStatusFor(runtimeLogFanouts, poolHostPort(rt.spec.Pool),
		rt.providerLogs.Load())
}
