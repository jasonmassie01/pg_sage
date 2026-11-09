package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	pool      *pgxpool.Pool // the latest database's pool, for re-detection
	status    logwatch.SourceStatus
	active    bool
	retryDone chan struct{} // closed when the retry goroutine ends
}

// logFanoutSet shares one log watcher per PostgreSQL cluster (host:port)
// across every runtime: databases on one cluster share its log directory.
type logFanoutSet struct {
	mu       sync.Mutex
	clusters map[string]*clusterLog
	// after is the retry clock; tests replace it.
	after func(time.Duration) <-chan time.Time
}

func newLogFanoutSet() *logFanoutSet {
	return &logFanoutSet{clusters: map[string]*clusterLog{}, after: time.After}
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
		if pool != nil {
			cl.pool = pool
		}
		return cl.fanout
	}
	watcher := logwatch.NewFileWatcher(cfg.LogWatch, logStructuredWrapper)
	cl := &clusterLog{watcher: watcher, pool: pool, retryDone: make(chan struct{}),
		fanout: logwatch.NewLogFanout(watcher).WithClusterKey(key)}
	s.clusters[key] = cl
	// The first attempt holds s.mu so every database of the cluster sees
	// its outcome; retries run outside it.
	if s.record(ctx, key, cl, startClusterLog(ctx, key, cl.watcher, pool)) {
		close(cl.retryDone)
		return cl.fanout
	}
	go s.retry(ctx, key, cl)
	return cl.fanout
}

// logStartTimeout bounds the pg_settings detection of one start attempt.
const logStartTimeout = 10 * time.Second

// startClusterLog detects the log source and starts the watcher once. Only
// the cluster's first call or its single retry goroutine calls it, never
// both at once, and before the watcher's drain loop exists.
func startClusterLog(
	ctx context.Context, key string, watcher *logwatch.FileWatcher, pool *pgxpool.Pool,
) logwatch.SourceStatus {
	detectCtx, cancel := context.WithTimeout(ctx, logStartTimeout)
	defer cancel()
	settings, err := resolvedLogWatchConfig(detectCtx, pool)
	if err != nil {
		return unavailableLog(key, fmt.Errorf("auto-detect: %w", err))
	}
	if err := watcher.StartWithConfig(ctx, settings); err != nil {
		return unavailableLog(key, fmt.Errorf("cannot read logwatch.log_directory %s "+
			"(%w): mount the server's log_directory read-only into the sidecar and "+
			"check that logging_collector = on and log_destination includes %s",
			settings.LogDirectory, err, settings.Format))
	}
	logInfo("logwatch", "[%s] started (dir=%s format=%s)",
		key, settings.LogDirectory, settings.Format)
	return logwatch.SourceStatus{State: logwatch.SourceActive, Format: settings.Format,
		Hints: logSettingHints(ctx, key, pool)}
}

func unavailableLog(key string, err error) logwatch.SourceStatus {
	logWarn("logwatch", "[%s] log source unavailable: %v", key, err)
	return logwatch.UnavailableStatus(err.Error())
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
		pool := cl.pool
		s.mu.Unlock()
		st := startClusterLog(ctx, key, cl.watcher, pool)
		s.mu.Lock()
		started := s.record(ctx, key, cl, st)
		s.mu.Unlock()
		if started {
			return
		}
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

// logSourceStatusFor is a database's log-source status: disabled with the
// setting that turns it off, else its cluster's status.
func logSourceStatusFor(set *logFanoutSet, key string) logwatch.SourceStatus {
	if !cfg.LogWatch.Enabled {
		return logwatch.DisabledStatus("logwatch.enabled is false")
	}
	if !logConsumerEnabled() {
		return logwatch.DisabledStatus("no log consumer: rca.enabled is false and " +
			"migration log detection is off")
	}
	st := set.status(key)
	if st.State == "" {
		return logwatch.UnavailableStatus("log watcher not started for this database")
	}
	return st
}

// logSourceStatus is this runtime's log-source status for the fleet API.
func (rt *databaseRuntime) logSourceStatus() logwatch.SourceStatus {
	return logSourceStatusFor(runtimeLogFanouts, poolHostPort(rt.spec.Pool))
}
