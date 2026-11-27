package migration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// Detector polls pg_stat_activity for active DDL statements and
// feeds them through the Advisor for risk assessment. Each DDL is
// analysed once per backend and statement, and re-evaluated on every poll
// against the latest lock-chain observation (migration-05).
type Detector struct {
	pool         *pgxpool.Pool
	advisor      *Advisor
	cfg          *config.MigrationConfig
	logFn        func(string, string, ...any)
	knownQueries map[int]string // pid -> last seen query
	findingSink  FindingSink
	stale        *StaleResolver
	lockQueue    LockQueueSource // nil: the lock queue is not observed

	// mu guards tracked, which the lock-chain probe reads (QueuedDDLPID).
	mu      sync.Mutex
	tracked map[int]trackedDDL

	// Seams for tests; production reads pg_stat_activity and the advisor.
	activity func(context.Context) ([]activeDDL, error)
	analyze  func(context.Context, activeDDL) (*rca.Incident, *DDLRisk, error)
}

// NewDetector creates a Detector for activity-based DDL detection.
func NewDetector(
	pool *pgxpool.Pool,
	advisor *Advisor,
	cfg *config.MigrationConfig,
	logFn func(string, string, ...any),
) *Detector {
	d := &Detector{
		pool:         pool,
		advisor:      advisor,
		cfg:          cfg,
		logFn:        logFn,
		knownQueries: make(map[int]string),
		tracked:      make(map[int]trackedDDL),
	}
	d.activity = d.queryActivity
	d.analyze = func(ctx context.Context, a activeDDL) (*rca.Incident, *DDLRisk, error) {
		return d.advisor.AnalyzeSessionRisk(ctx, a.Query, a.Role, a.PID)
	}
	return d
}

// WithFindingSink persists detected migration findings.
func (d *Detector) WithFindingSink(sink FindingSink) *Detector {
	d.findingSink = sink
	return d
}

// WithStaleResolver gives the detector the database's shared resolver of
// stale findings; the log detector gets the same one.
func (d *Detector) WithStaleResolver(r *StaleResolver) *Detector {
	d.stale = r
	return d
}

// WithLockQueue gives the detector the database's lock-chain observation
// (analyzer.LockChainObserver), so the queue behind a waiting DDL is seen.
func (d *Detector) WithLockQueue(src LockQueueSource) *Detector {
	d.lockQueue = src
	return d
}

// StaleResolver is the resolver the detector runs (nil when none).
func (d *Detector) StaleResolver() *StaleResolver { return d.stale }

// ddlActivitySQL finds DDL running in THIS database (pg_stat_activity is
// cluster-wide, G7-B17), excluding pg_sage's own backends (G7-B27).
// PostgreSQL ARE uses \y as the word boundary; \b is a backspace, which
// made this query match nothing (G7-B02). Leading comments are allowed.
// The wait state and backend identity let a DDL waiting for its lock be
// matched to the lock-chain observation (migration-05).
const ddlActivitySQL = `
SELECT pid, query, COALESCE(wait_event_type, ''), backend_start,
       query_start, COALESCE(usename, '')
FROM   pg_stat_activity
WHERE  state = 'active'
  AND  pid != pg_backend_pid()
  AND  datname = current_database()
  AND  COALESCE(application_name, '') NOT ILIKE '%pg_sage%'
  AND  query ~* '^\s*(/\*.*?\*/\s*|--[^\n]*\n\s*)*` +
	`(ALTER|CREATE\s+(UNIQUE\s+)?INDEX|DROP|REINDEX|VACUUM|REFRESH|CLUSTER)\y'`

// queryActivity reads the in-flight DDL; a row that fails to scan is
// logged and skipped.
func (d *Detector) queryActivity(ctx context.Context) ([]activeDDL, error) {
	rows, err := d.pool.Query(ctx, ddlActivitySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activeDDL
	for rows.Next() {
		var a activeDDL
		var backendStart, queryStart *time.Time
		if err := rows.Scan(&a.PID, &a.RawQuery, &a.WaitEventType,
			&backendStart, &queryStart, &a.Role); err != nil {
			d.logFn("warn", "migration: scan error: %v", err)
			continue
		}
		if backendStart != nil {
			a.BackendStart = *backendStart
		}
		if queryStart != nil {
			a.QueryStart = *queryStart
		}
		a.Query = strings.TrimSpace(a.RawQuery)
		out = append(out, a)
	}
	return out, rows.Err()
}

// PollOnce reads the in-flight DDL, analyses new statements and
// re-persists every risky one with the latest lock-queue evidence, which
// also moves its last_seen forward. It returns the newly analysed
// incidents.
func (d *Detector) PollOnce(
	ctx context.Context,
) ([]*rca.Incident, error) {
	if !d.activityPollingEnabled() {
		return nil, nil
	}
	ddls, err := d.activity(ctx)
	if err != nil {
		return nil, fmt.Errorf("read ddl activity: %w", err)
	}
	currentPIDs := make(map[int]bool, len(ddls))
	var incidents []*rca.Incident
	for _, a := range ddls {
		currentPIDs[a.PID] = true
		if inc := d.evaluate(ctx, a); inc != nil {
			incidents = append(incidents, inc)
		}
	}
	d.pruneStale(currentPIDs)
	return incidents, nil
}

// evaluate analyses a new DDL (or reuses the analysis of one already
// seen), applies the lock-queue verdict and persists the finding. A new
// analysis then queues its model work, which never runs on the poll path
// (migration-10). It returns the incident only when it was analysed on
// this poll.
func (d *Detector) evaluate(ctx context.Context, a activeDDL) *rca.Incident {
	inc, risk, fresh := d.incidentFor(ctx, a)
	analysed := inc
	if a.waitingOnLock() && d.lockQueue != nil {
		d.lockQueue.Nudge()
	}
	v := d.lockQueueVerdict(a)
	if inc == nil && v.Escalate {
		// Tracked like an analysis, so later polls re-upsert it and its
		// severity follows the queue as it drains.
		inc = d.queueIncident(a)
		d.track(a, inc, nil)
	}
	if inc != nil {
		d.persist(ctx, a, inc, risk, v)
	}
	if !fresh {
		return nil
	}
	d.advisor.queueEnrichment(a.PID, a.Query, analysed, risk, d.findingSink)
	return analysed
}

// incidentFor returns the DDL's analysis and its assessment, running the
// advisor only for a new backend or statement, and whether it ran and
// succeeded on this call; a failed analysis is logged and not retried for
// that statement.
func (d *Detector) incidentFor(
	ctx context.Context, a activeDDL,
) (*rca.Incident, *DDLRisk, bool) {
	d.mu.Lock()
	prev, seen := d.tracked[a.PID]
	d.mu.Unlock()
	if seen && d.knownQueries[a.PID] == a.Query &&
		prev.ddl.BackendStart.Equal(a.BackendStart) {
		d.track(a, prev.inc, prev.risk)
		return prev.inc, prev.risk, false
	}
	d.knownQueries[a.PID] = a.Query
	inc, risk, err := d.analyze(ctx, a)
	if err != nil {
		d.logFn("warn", "migration: analysis failed for pid %d: %v", a.PID, err)
		d.track(a, nil, nil)
		return nil, nil, false
	}
	d.track(a, inc, risk)
	return inc, risk, true
}

func (d *Detector) track(a activeDDL, inc *rca.Incident, risk *DDLRisk) {
	d.mu.Lock()
	if d.tracked == nil {
		d.tracked = make(map[int]trackedDDL)
	}
	d.tracked[a.PID] = trackedDDL{ddl: a, inc: inc, risk: risk}
	d.mu.Unlock()
}

// persistFinding persists inc for pid with no assessment and no
// lock-queue evidence.
func (d *Detector) persistFinding(
	ctx context.Context,
	pid int,
	query string,
	inc *rca.Incident,
) {
	d.persist(ctx, activeDDL{PID: pid, Query: query}, inc, nil, lockQueueVerdict{})
}

// persist upserts the finding for inc with risk's detection-time
// measurements (nil: none) and the lock-queue verdict.
func (d *Detector) persist(
	ctx context.Context, a activeDDL, inc *rca.Incident, risk *DDLRisk,
	v lockQueueVerdict,
) {
	if d.findingSink == nil {
		return
	}
	finding, ok := FindingFromIncident(a.PID, a.Query, inc, risk)
	if !ok {
		return
	}
	applyLockQueue(&finding, a, v)
	if _, err := d.findingSink.UpsertMigrationSafetyFinding(
		ctx, finding); err != nil {
		d.logFn("warn", "migration: persist finding failed: %v", err)
	}
}

// pruneStale removes entries for PIDs that are no longer active.
func (d *Detector) pruneStale(currentPIDs map[int]bool) {
	for pid := range d.knownQueries {
		if !currentPIDs[pid] {
			delete(d.knownQueries, pid)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for pid := range d.tracked {
		if !currentPIDs[pid] {
			delete(d.tracked, pid)
		}
	}
}

// Run starts the polling loop, blocking until ctx is cancelled.
func (d *Detector) Run(ctx context.Context) {
	if !d.activityPollingEnabled() {
		return
	}
	interval := time.Duration(d.cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	d.logFn("info", "migration: detector started (poll=%s)", interval)

	for {
		select {
		case <-ctx.Done():
			d.logFn("info", "migration: detector stopped")
			return
		case <-ticker.C:
			d.stale.maybeRun(ctx)
			incidents, err := d.PollOnce(ctx)
			if err != nil {
				d.logFn("warn",
					"migration: poll error: %v", err)
				continue
			}
			for _, inc := range incidents {
				d.logFn("warn",
					"migration: DDL risk detected: %s (score=%.2f)",
					inc.RootCause, inc.Confidence)
			}
		}
	}
}

func (d *Detector) activityPollingEnabled() bool {
	return d != nil && d.cfg != nil && d.cfg.Enabled && d.cfg.ActivityPolling
}
