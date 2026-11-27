package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// startInstanceWorker registers a goroutine with the database runtime before
// starting it, so removal can cancel and drain every instance-owned worker.
func startInstanceWorker(workers *sync.WaitGroup, run func()) {
	workers.Add(1)
	go func() {
		defer workers.Done()
		run()
	}()
}

// resetFleetBudgetDaily resets the per-database LLM token budget at each
// UTC midnight (F5). A 24h ticker drifted with process start time.
func resetFleetBudgetDaily(ctx context.Context, b *fleet.FleetBudget) {
	for {
		timer := time.NewTimer(durationUntilNextUTCMidnight(time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			b.ResetDaily()
		}
	}
}

// durationUntilNextUTCMidnight is strictly positive: at exactly midnight the
// next reset is 24h away.
func durationUntilNextUTCMidnight(now time.Time) time.Duration {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(utc)
}

// statusQueryTimeout bounds each status read on the monitored database.
const statusQueryTimeout = 5 * time.Second

// openIncidentsSQL counts unresolved incidents by severity (cases-11).
const openIncidentsSQL = `SELECT severity, count(*)
	   FROM sage.incidents
	  WHERE resolved_at IS NULL
	  GROUP BY severity`

func updateInstanceFindings(
	ctx context.Context,
	inst *fleet.DatabaseInstance,
) {
	refreshCollectionStatus(ctx, inst)
	dbPool := inst.Pool
	if dbPool == nil {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, statusQueryTimeout)
	defer cancel()
	counts, err := readFindingCounts(queryCtx, dbPool)
	if err != nil {
		logWarn("fleet", "db %q: findings query: %v", inst.Name, err)
		// Incidents were not refreshed either: never leave a stale zero.
		inst.UpdateStatus(func(s *fleet.InstanceStatus) {
			applyIncidentCounts(s, severityCounts{}, err)
		})
		markInstanceConnectivity(ctx, inst, dbPool)
		return
	}
	incidents, incErr := readIncidentCounts(queryCtx, dbPool)
	if incErr != nil {
		logWarn("fleet", "db %q: incident counts unknown: %v", inst.Name, incErr)
	}
	now := time.Now()
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		s.FindingsOpen = counts.open
		s.FindingsCritical = counts.critical
		s.FindingsWarning = counts.warning
		s.FindingsInfo = counts.info
		applyIncidentCounts(s, incidents, incErr)
		s.AnalyzerLastRun = now
		s.LastSeen = now
		s.Connected = true
		s.Error = ""
		if fleetLLMBudget != nil {
			s.LLMTokensUsed = fleetLLMBudget.Used(inst.Name)
		}
	})
}

// statusQuerier is the part of a pool the status reads use.
type statusQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readFindingCounts(ctx context.Context, q statusQuerier) (severityCounts, error) {
	rows, err := q.Query(ctx,
		`SELECT severity, count(*)
		   FROM sage.findings
		  WHERE status = 'open'
		  GROUP BY severity`)
	if err != nil {
		return severityCounts{}, err
	}
	defer rows.Close()
	return scanSeverityCounts(rows), nil
}

// readIncidentCounts counts unresolved incidents by severity under the
// status query timeout. Any error, including one that ends the rows
// early, is returned so the caller records the counts as unknown.
func readIncidentCounts(ctx context.Context, q statusQuerier) (severityCounts, error) {
	queryCtx, cancel := context.WithTimeout(ctx, statusQueryTimeout)
	defer cancel()
	rows, err := q.Query(queryCtx, openIncidentsSQL)
	if err != nil {
		return severityCounts{}, fmt.Errorf("open incidents query: %w", err)
	}
	defer rows.Close()
	counts := scanSeverityCounts(rows)
	if err := rows.Err(); err != nil {
		return severityCounts{}, fmt.Errorf("reading open incidents: %w", err)
	}
	return counts, nil
}

// applyIncidentCounts records the read counts, or IncidentsUnknown for
// both when the read failed.
func applyIncidentCounts(s *fleet.InstanceStatus, c severityCounts, err error) {
	if err != nil {
		s.IncidentsCritical = fleet.IncidentsUnknown
		s.IncidentsWarning = fleet.IncidentsUnknown
		return
	}
	s.IncidentsCritical = c.critical
	s.IncidentsWarning = c.warning
}

// severityCounts holds open findings by severity; open is the total.
type severityCounts struct {
	open, critical, warning, info int
}

// scanSeverityCounts reads (severity, count) rows; unscannable rows are
// skipped.
func scanSeverityCounts(rows pgx.Rows) severityCounts {
	var c severityCounts
	for rows.Next() {
		var sev string
		var cnt int
		if err := rows.Scan(&sev, &cnt); err != nil {
			continue
		}
		c.open += cnt
		switch sev {
		case "critical":
			c.critical = cnt
		case "warning":
			c.warning = cnt
		case "info":
			c.info = cnt
		}
	}
	return c
}

// markInstanceConnectivity distinguishes a down database from a query
// error: only a failed ping flips the instance to disconnected, so a
// missing table does not trigger meta-mode reconnect churn (G5-B12).
func markInstanceConnectivity(
	ctx context.Context, inst *fleet.DatabaseInstance, dbPool *pgxpool.Pool,
) {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pingErr := dbPool.Ping(pingCtx)
	if pingErr == nil {
		return
	}
	logWarn("fleet", "db %q: unreachable: %v", inst.Name, pingErr)
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		s.Connected = false
		s.Error = fmt.Sprintf("unreachable: %v", pingErr)
	})
}
