package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Restore drills (drill-01a): every hour the fleet leader reads each
// database's standing policy drill block and starts the drills that are
// due on clone.provider (or the drill.command restore command, drill-01c),
// at most drill.max_concurrent_per_fleet at once.
// Without either the database gets the info finding
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
	if line := restoreCommandStartupLine(c.Drill.Command); line != "" {
		logInfo("drill", "%s", line)
	}
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
	// runtime is the runtimes' control database (databaseRuntimeSpec's
	// ControlPool): the standing policy the executor enforces and the
	// notification routes the database's alerts use live there.
	runtime *pgxpool.Pool
	// policyScoped: the standing policy is per database (meta mode); in
	// standalone and YAML fleet the executor enforces the global one.
	policyScoped bool
}

// runtimeControlPool is the control database every runtime is built with:
// the meta database, else the fleet's primary (standalone: the one
// database). It is the API's auth pool, where operators edit the policy
// and the notification routes.
func runtimeControlPool(mgr *fleet.DatabaseManager, meta *metaDBState) *pgxpool.Pool {
	if meta != nil && meta.Pool != nil {
		return meta.Pool
	}
	return mgr.PoolForDatabase("all")
}

// restoreDrillResults is where the API and MCP read a database's drill
// rows (drill-01d): the control database drills record in, as
// drillInstances resolves it, or nil for the database's own sage schema.
// It is resolved per request, so a control database that connects later
// is read once it is up.
func restoreDrillResults(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) func(string) *pgxpool.Pool {
	return func(string) *pgxpool.Pool { return controlPoolOf(c, mgr, meta) }
}

// mcpRestoreDrills serves MCP restore_drills from the API's read.
func mcpRestoreDrills(mgr *fleet.DatabaseManager) mcp.RestoreDrillsBackend {
	return api.RestoreDrillsMCP(mgr, cfg, restoreDrillResults(cfg, mgr, globalMetaState))
}

func drillInstances(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) []drillInstance {
	control := governanceControlPool(c, mgr, meta)
	runtime := runtimeControlPool(mgr, meta)
	scoped := meta != nil && meta.Pool != nil
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
			results: results, runtime: runtime, policyScoped: scoped})
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
		pager := sharedNotifyDispatcher(inst.runtime)
		if !c.Drill.Command.Configured() &&
			(cloneCfg.Provider == "" || cloneCfg.Provider == "none") {
			reportRecoverabilityUnknown(ctx, inst, pager, "clone.provider is none and "+
				"drill.command.restore_argv is not set")
			continue
		}
		t, err := drillTarget(ctx, c, cloneCfg, inst, doc, pager)
		if err != nil {
			logWarn("drill", "restore drill of %s skipped this pass: %v", inst.name, err)
			continue
		}
		if err := t.KindAllowed(); err != nil {
			reportRecoverabilityUnknown(ctx, inst, pager, "no restore drill can run: "+
				err.Error())
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

// drillPolicyOf is the drill block of the standing policy the database's
// executor enforces (policyStoreScope); the defaults when it has no
// standing policy yet.
func drillPolicyOf(ctx context.Context, inst drillInstance) (policy.DrillPolicy, error) {
	var scope policy.Scope
	if inst.policyScoped && inst.id > 0 {
		id := int64(inst.id)
		scope.DatabaseID = &id
	}
	if inst.runtime == nil {
		return policy.DrillPolicy{}, errors.New("no control database to read the policy")
	}
	current, err := policy.NewStore(inst.runtime).Current(ctx, scope)
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
