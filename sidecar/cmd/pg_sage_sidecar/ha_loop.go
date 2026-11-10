package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ha"
)

// One HA monitor per database (fleet-04). It is built with the runtime and
// persisted in the control database; the HA loop checks it at a fixed
// cadence and the executor checks it just before each cycle. The proposal
// router, the runway advisor, earned autonomy and the fleet status read
// its snapshot and never probe, so they agree on the role, the flip
// history, safe mode and the failover cooldown.

// haCheckTimeout bounds every check of the monitored database.
const haCheckTimeout = 5 * time.Second

// newInstanceHAMonitor is the only HA monitor constructor of a database.
// With a control pool its identity history is persisted there under the
// database's stable key, so a failover while pg_sage was down opens the
// cooldown at startup. cooldown is earned autonomy's failover cooldown
// (sre.autonomy.failover_cooldown_minutes), which every consumer applies
// whatever sre.autonomy.enforce says. Without a monitored pool there is
// no monitor.
func newInstanceHAMonitor(monitored, control *pgxpool.Pool, database string,
	databaseID *int, cadence, cooldown time.Duration) *ha.Monitor {
	if monitored == nil {
		return nil
	}
	m := ha.New(monitored, logStructuredWrapper).WithCadence(cadence).
		WithFailoverCooldown(cooldown)
	if control == nil {
		return m
	}
	return m.WithIdentityStore(ha.NewPostgresIdentityStore(control),
		haIdentityKey(database, databaseID))
}

// orchestratorInterval is the executor, briefing and retention cycle.
func orchestratorInterval(c *config.Config) time.Duration {
	return c.Analyzer.Interval() + 5*time.Second
}

// haCadence is ha.check_interval_seconds, at most the orchestrator
// interval, so a snapshot is never older than one executor cycle.
func haCadence(c *config.Config) time.Duration {
	if c == nil {
		return ha.DefaultCheckInterval
	}
	cadence := c.HA.CheckInterval()
	if limit := orchestratorInterval(c); limit > 0 && limit < cadence {
		return limit
	}
	return cadence
}

// checkHA checks m once, bounded by haCheckTimeout, and reports whether
// the node must not be treated as a writable primary (a missing monitor
// or a failed probe: true).
func checkHA(ctx context.Context, m *ha.Monitor) bool {
	if m == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, haCheckTimeout)
	defer cancel()
	return m.Check(ctx)
}

// runHALoop checks m at its cadence until ctx ends.
func runHALoop(ctx context.Context, m *ha.Monitor) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(m.Cadence())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkHA(ctx, m)
		}
	}
}

// haNotWritable reads m's snapshot: true unless the node is a confirmed
// primary, outside safe mode and past its failover cooldown. A missing
// monitor, or one the loop stopped checking, is not writable.
func haNotWritable(m *ha.Monitor) bool {
	return !m.Snapshot().Writable(time.Now(), m.FailoverCooldown())
}

// haReplicaGate is the isReplica signal of the router and the runway
// advisor: the shared snapshot, never a probe of its own.
func haReplicaGate(m *ha.Monitor) func(context.Context) bool {
	return func(context.Context) bool { return haNotWritable(m) }
}

// startHA checks the database once, so consumers built next read a known
// role, then runs the HA loop on the instance worker group.
func (rt *databaseRuntime) startHA() {
	if rt.ha == nil {
		return
	}
	if checkHA(rt.ctx, rt.ha) {
		logWarn(rt.spec.Scope, "db %q: not a confirmed writable primary at startup; "+
			"autonomous actions are held until it is", rt.spec.Name)
	}
	m := rt.ha
	rt.start(func() { runHALoop(rt.ctx, m) })
	rt.note("ha")
}
