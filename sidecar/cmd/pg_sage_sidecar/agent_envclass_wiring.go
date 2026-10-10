package main

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/leader"
)

// Agent governance (spec §6.5, §6.7): environment binding and column
// classification wiring. Governance state lives in the control database:
// the meta database in meta mode, else the monitored database pinned by
// agents.control_database. Without either it is posture-only.

const (
	// agentEnvReconcileInterval matches agents.reconcile_interval_seconds'
	// spec default (60 s).
	agentEnvReconcileInterval = time.Minute
	agentClassScanInterval    = 24 * time.Hour
	agentClassFirstScanDelay  = 3 * time.Minute
	// agentClassMaxPages bounds one daily scan per database (500 columns
	// a page); the cursor carries on the next day.
	agentClassMaxPages = 40
)

var (
	agentEnvSvc        *envbind.Service
	agentPostureOnce   sync.Once
	agentClassSeen     = classify.NewSeen()
	agentClassCursorMu sync.Mutex
	agentClassCursors  = map[string]classify.Cursor{}
	// agentEnvFailures is the last reconcile failure logged per database.
	agentEnvFailures sync.Map
)

// governanceControlPool is agent governance's control database; nil means
// posture-only, logged once.
func governanceControlPool(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) *pgxpool.Pool {
	if meta != nil && meta.Pool != nil {
		return meta.Pool
	}
	if c != nil && c.Agents.ControlDatabase != "" && mgr != nil {
		if inst := mgr.GetInstance(c.Agents.ControlDatabase); inst != nil && inst.Pool != nil {
			return inst.Pool
		}
		agentPostureOnce.Do(func() {
			logWarn("agents", "agents.control_database %q is not a connected monitored "+
				"database: agent governance runs posture checks only", c.Agents.ControlDatabase)
		})
		return nil
	}
	agentPostureOnce.Do(func() {
		logInfo("agents", "agent governance runs posture checks only: set "+
			"agents.control_database (or use mode: meta) to bind environments")
	})
	return nil
}

// agentEnvironmentService builds the process's environment service once
// the fleet is registered.
func agentEnvironmentService(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) *envbind.Service {
	control := governanceControlPool(c, mgr, meta)
	var receipts envbind.ReceiptSource
	if control != nil {
		receipts = clone.NewReceiptStore(control)
	}
	agentEnvSvc = &envbind.Service{Binder: envbind.NewBinder(control, receipts),
		Resolve: agentEnvResolver(mgr)}
	return agentEnvSvc
}

// agentEnvResolver maps a fleet name to the binder's view of it: the SRE
// binding's database_id and the cloud telemetry resource id.
func agentEnvResolver(mgr *fleet.DatabaseManager) envbind.Resolver {
	return func(_ context.Context, name string) (envbind.Database, error) {
		var inst *fleet.DatabaseInstance
		if mgr != nil {
			inst = mgr.GetInstance(name)
		}
		if inst == nil || inst.Pool == nil {
			return envbind.Database{}, envbind.ErrUnknownDatabase
		}
		db := envbind.Database{Name: name, Pool: inst.Pool, ProviderRef: providerRefOf(name)}
		if inst.Investigations != nil {
			if scope, ok := inst.Investigations.Coordinator().Scope(); ok {
				db.ID = string(scope.DatabaseID)
			}
		}
		return db, nil
	}
}

func providerRefOf(name string) string {
	rt := cloudtel.Lookup(name)
	if rt == nil {
		return ""
	}
	if st := rt.Status(); st.Resource != "" {
		return st.Provider + ":" + st.Resource
	}
	return ""
}

// startAgentGovernance starts the environment reconciler and the daily
// classification scan as fleet-wide jobs on the leader. The lease is the
// fleet leader's when governance shares its control database; with a
// pinned agents.control_database it is taken in that database, so
// leadership and governance state never sit in different databases.
func startAgentGovernance(ctx context.Context, fleetControl *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	svc := agentEnvSvc
	if svc == nil || svc.Binder == nil || cfg == nil {
		return
	}
	control := governanceControlPool(cfg, mgr, globalMetaState)
	if control == nil {
		return
	}
	lead := agentGovernanceLeader(ctx, control, fleetControl)
	// Retire, broker rotation and the backend check (agent_upkeep_wiring.go).
	startAgentUpkeep(ctx, mgr, control, lead)
	go every(ctx, agentEnvReconcileInterval, func(c context.Context) {
		runAgentEnvReconcile(c, svc, mgr, lead)
	})
	go afterDelay(ctx, agentClassFirstScanDelay, func() {
		every(ctx, agentClassScanInterval, func(c context.Context) {
			runAgentClassScan(c, mgr, lead)
		})
	})
	logInfo("agents", "agent governance: environment reconciler and classification "+
		"scan started")
}

// governanceLeader decides and fences the governance jobs.
type governanceLeader struct {
	elector *leader.Elector
	scope   string
}

func agentGovernanceLeader(ctx context.Context, control,
	fleetControl *pgxpool.Pool) governanceLeader {
	scope := fleetLearningScope(cfg)
	if control == fleetControl || cfg.FleetLearning.LeaderLeaseSeconds <= 0 {
		return governanceLeader{elector: fleetLeader, scope: scope}
	}
	scope = "agents:" + scope
	e := leader.NewElector(leader.NewPostgresStore(control), scope, leader.HolderID(),
		cfg.FleetLearning.LeaderLease(), leader.WithLogger(logStructuredWrapper))
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := e.Tick(tctx); err != nil {
		logWarn("agents", "governance lease: first attempt failed, retrying: %v", err)
	}
	cancel()
	go e.Run(ctx)
	return governanceLeader{elector: e, scope: scope}
}

// fence returns the lease to write under; ok is false on a follower.
func (g governanceLeader) fence(job string) (envbind.Fence, bool) {
	if g.elector == nil {
		return envbind.Fence{}, fleetLeaderAllows(job)
	}
	holder, epoch, ok := g.elector.Fence()
	if !ok {
		return envbind.Fence{}, false
	}
	return envbind.Fence{Scope: g.scope, Holder: holder, Epoch: epoch}, true
}

func governedDatabases(ctx context.Context, svc *envbind.Service,
	mgr *fleet.DatabaseManager) []envbind.Database {
	var out []envbind.Database
	for name := range mgr.Instances() {
		db, err := svc.Resolve(ctx, name)
		if err == nil {
			out = append(out, db)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func runAgentEnvReconcile(ctx context.Context, svc *envbind.Service,
	mgr *fleet.DatabaseManager, lead governanceLeader) {
	fence, ok := lead.fence("agent environment reconcile")
	if !ok || mgr == nil {
		return
	}
	dbs := governedDatabases(ctx, svc, mgr)
	rep, err := svc.Binder.Reconcile(ctx, dbs, fence)
	if errors.Is(err, envbind.ErrFenced) {
		logWarn("agents", "environment reconcile stopped: the lease moved mid-pass")
		return
	}
	if err != nil {
		logWarn("agents", "environment reconcile failed: %v", err)
		return
	}
	for name, ferr := range rep.Failed {
		// Logged when the reason changes, not every minute.
		if prev, seen := agentEnvFailures.Swap(name, ferr.Error()); !seen ||
			prev != ferr.Error() {
			logWarn("agents", "environment of %s not observed (evaluated as prod): %v",
				name, ferr)
		}
	}
	for _, db := range dbs {
		if _, failed := rep.Failed[db.Name]; !failed {
			agentEnvFailures.Delete(db.Name)
		}
	}
	for _, name := range rep.Critical {
		logWarn("agents", "environment binding of %s no longer holds: evaluated as prod "+
			"(critical finding raised)", name)
	}
}
