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

func updateInstanceFindings(
	ctx context.Context,
	inst *fleet.DatabaseInstance,
) {
	refreshCollectionStatus(ctx, inst)
	dbPool := inst.Pool
	if dbPool == nil {
		return
	}
	rows, err := dbPool.Query(ctx,
		`SELECT severity, count(*)
		   FROM sage.findings
		  WHERE status = 'open'
		  GROUP BY severity`)
	if err != nil {
		logWarn("fleet", "db %q: findings query: %v", inst.Name, err)
		markInstanceConnectivity(ctx, inst, dbPool)
		return
	}
	defer rows.Close()

	counts := scanSeverityCounts(rows)
	now := time.Now()
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		s.FindingsOpen = counts.open
		s.FindingsCritical = counts.critical
		s.FindingsWarning = counts.warning
		s.FindingsInfo = counts.info
		s.AnalyzerLastRun = now
		s.LastSeen = now
		s.Connected = true
		s.Error = ""
		if fleetLLMBudget != nil {
			s.LLMTokensUsed = fleetLLMBudget.Used(inst.Name)
		}
	})
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
