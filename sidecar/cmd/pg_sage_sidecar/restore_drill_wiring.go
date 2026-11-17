package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Restore drills (drill-01a): every hour the fleet leader reads each
// database's standing policy drill block and starts the drills that are
// due on clone.provider, at most drill.max_concurrent_per_fleet at once.
// Without a provider the database gets the info finding
// recoverability_unknown instead. Drill clones are receipted (purpose
// drill) and reaped by the clone reaper; results live where receipts live.

const restoreDrillInterval = time.Hour

// startRestoreDrills starts the drill loop. It keeps the clone config it
// started with, as the clone reaper does, so drills and the reaper always
// name the same provider endpoint.
func startRestoreDrills(ctx context.Context, c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) {
	if c == nil || mgr == nil {
		return
	}
	cloneCfg := c.Clone
	sched := &drill.Scheduler{MaxConcurrent: c.Drill.MaxConcurrent(),
		Logf: func(format string, args ...any) { logWarn("drill", format, args...) }}
	go every(ctx, restoreDrillInterval, func(ctx context.Context) {
		restoreDrillPass(ctx, c, cloneCfg, mgr, meta, sched)
	})
	logInfo("drill", "restore drill scheduler started (every %s, at most %d at once)",
		restoreDrillInterval, sched.MaxConcurrent)
}

// restoreDrillPass runs one scheduler pass on the leader; it returns the
// databases whose drills it started.
func restoreDrillPass(ctx context.Context, c *config.Config,
	cloneCfg config.CloneProviderConfig, mgr *fleet.DatabaseManager, meta *metaDBState,
	sched *drill.Scheduler) []string {
	if !fleetLeaderAllows("restore drills") {
		return nil
	}
	targets, history := restoreDrillTargets(ctx, c, cloneCfg, mgr, meta)
	sched.History = history
	return sched.Pass(ctx, targets)
}

// drillHistories routes each database's drill history to the store its
// drills are recorded in.
type drillHistories map[string]*drill.Store

func (h drillHistories) LastAttempt(ctx context.Context, database string) (drill.Attempt,
	bool, error) {
	s, ok := h[database]
	if !ok {
		return drill.Attempt{}, false, fmt.Errorf("no drill store for %s", database)
	}
	return s.LastAttempt(ctx, database)
}

// drillInstance is one database the pass considers.
type drillInstance struct {
	name    string
	id      int
	pool    *pgxpool.Pool
	results *pgxpool.Pool // where receipts and drill rows live
}

func drillInstances(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) []drillInstance {
	control := governanceControlPool(c, mgr, meta)
	instances := mgr.Instances()
	names := make([]string, 0, len(instances))
	for name, inst := range instances {
		if inst != nil && inst.Pool != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]drillInstance, 0, len(names))
	for _, name := range names {
		inst := instances[name]
		results := control
		if results == nil {
			results = inst.Pool
		}
		out = append(out, drillInstance{name: name, id: inst.DatabaseID, pool: inst.Pool,
			results: results})
	}
	return out
}

// restoreDrillTargets builds one target per database whose policy turns
// drills on and whose provider can be built; a database without a
// provider gets recoverability_unknown.
func restoreDrillTargets(ctx context.Context, c *config.Config,
	cloneCfg config.CloneProviderConfig, mgr *fleet.DatabaseManager,
	meta *metaDBState) ([]drill.Target, drillHistories) {
	history := drillHistories{}
	var targets []drill.Target
	for _, inst := range drillInstances(c, mgr, meta) {
		doc, err := drillPolicyOf(ctx, inst)
		if err != nil {
			logWarn("drill", "restore drill of %s skipped this pass: %v", inst.name, err)
			continue
		}
		if !doc.On() {
			continue
		}
		pager := sharedNotifyDispatcher(notificationControlPool(meta, inst.pool))
		if cloneCfg.Provider == "" || cloneCfg.Provider == "none" {
			reportRecoverabilityUnknown(ctx, inst, pager, "clone.provider is none")
			continue
		}
		t, err := drillTarget(ctx, c, cloneCfg, inst, doc, pager)
		if err != nil {
			logWarn("drill", "restore drill of %s skipped this pass: %v", inst.name, err)
			continue
		}
		history[inst.name] = drill.NewStore(inst.results)
		targets = append(targets, t)
	}
	return targets, history
}

func reportRecoverabilityUnknown(ctx context.Context, inst drillInstance,
	pager *notify.Dispatcher, reason string) {
	if err := drill.ReportUnknown(ctx, inst.pool, drillPager(pager), inst.name,
		reason); err != nil {
		logWarn("drill", "recoverability finding for %s: %v", inst.name, err)
	}
}

// drillPager is d as a drill.Pager: a nil dispatcher is no pager, never a
// typed nil.
func drillPager(d *notify.Dispatcher) drill.Pager {
	if d == nil {
		return nil
	}
	return d
}

// drillPolicyOf is the database's standing policy drill block; the
// defaults when it has no standing policy yet.
func drillPolicyOf(ctx context.Context, inst drillInstance) (policy.DrillPolicy, error) {
	var scope policy.Scope
	if inst.id > 0 {
		id := int64(inst.id)
		scope.DatabaseID = &id
	}
	current, err := policy.NewStore(inst.results).Current(ctx, scope)
	if errors.Is(err, policy.ErrNotFound) {
		return policy.DrillPolicy{}, nil
	}
	if err != nil {
		return policy.DrillPolicy{}, fmt.Errorf("read the standing policy: %w", err)
	}
	doc, err := policy.DocumentFromPolicy(current)
	if err != nil {
		return policy.DrillPolicy{}, err
	}
	return doc.Drill, nil
}
