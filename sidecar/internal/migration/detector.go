package migration

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// Detector polls pg_stat_activity for active DDL statements and
// feeds them through the Advisor for risk assessment.
type Detector struct {
	pool         *pgxpool.Pool
	advisor      *Advisor
	cfg          *config.MigrationConfig
	logFn        func(string, string, ...any)
	knownQueries map[int]string // pid -> last seen query
	findingSink  FindingSink
}

// NewDetector creates a Detector for activity-based DDL detection.
func NewDetector(
	pool *pgxpool.Pool,
	advisor *Advisor,
	cfg *config.MigrationConfig,
	logFn func(string, string, ...any),
) *Detector {
	return &Detector{
		pool:         pool,
		advisor:      advisor,
		cfg:          cfg,
		logFn:        logFn,
		knownQueries: make(map[int]string),
	}
}

func (d *Detector) WithFindingSink(sink FindingSink) *Detector {
	d.findingSink = sink
	return d
}

const ddlActivitySQL = `
SELECT pid, query
FROM   pg_stat_activity
WHERE  state = 'active'
  AND  pid != pg_backend_pid()
  AND  (
    query ~* '^\s*(ALTER|CREATE\s+INDEX|DROP|REINDEX|VACUUM|REFRESH|CLUSTER)\b'
  )`

// PollOnce queries pg_stat_activity for DDL and analyzes new ones.
func (d *Detector) PollOnce(
	ctx context.Context,
) ([]*rca.Incident, error) {
	rows, err := d.pool.Query(ctx, ddlActivitySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	currentPIDs := make(map[int]bool)
	var incidents []*rca.Incident

	for rows.Next() {
		var pid int
		var query string
		if err := rows.Scan(&pid, &query); err != nil {
			d.logFn("warn", "migration: scan error: %v", err)
			continue
		}
		currentPIDs[pid] = true
		query = strings.TrimSpace(query)

		if prev, seen := d.knownQueries[pid]; seen && prev == query {
			continue // already analyzed this query from this pid
		}
		d.knownQueries[pid] = query

		inc, err := d.advisor.Analyze(ctx, query)
		if err != nil {
			d.logFn("warn",
				"migration: analysis failed for pid %d: %v", pid, err)
			continue
		}
		if inc != nil {
			incidents = append(incidents, inc)
			d.persistFinding(ctx, pid, query, inc)
		}
	}

	// Prune PIDs no longer active.
	d.pruneStale(currentPIDs)

	return incidents, rows.Err()
}

func (d *Detector) persistFinding(
	ctx context.Context,
	pid int,
	query string,
	inc *rca.Incident,
) {
	if d.findingSink == nil {
		return
	}
	finding, ok := FindingFromIncident(pid, query, inc)
	if !ok {
		return
	}
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
}

// Run starts the polling loop, blocking until ctx is cancelled.
func (d *Detector) Run(ctx context.Context) {
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
