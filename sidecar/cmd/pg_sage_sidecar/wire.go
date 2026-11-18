package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/rollout"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/store"
)

// WireParams captures all inputs for building the API router.
// Each field corresponds to a global variable or startup artifact
// that startAPIServer previously read directly.
type WireParams struct {
	DatabricksAppAuth *auth.DatabricksAppConfig
	Cfg               *config.Config
	Pool              *pgxpool.Pool // global pool (standalone or meta-db)
	FleetMgr          *fleet.DatabaseManager
	LLMMgr            *llm.Manager
	MetaState         *metaDBState
	Actions           struct {
		Store    *store.ActionStore
		Executor *executor.Executor
	}
	RateLimiter *RateLimiter
	Config      *config.ConfigController
	ConfigBase  *config.Config
	// ConfigBaseLoader reloads the current file config for override
	// deletes (G5-B03); nil falls back to ConfigBase.
	ConfigBaseLoader func() (*config.Config, error)
	// LLMBudgets covers every LLM client and the fleet budget (G3-B14).
	LLMBudgets api.LLMBudgetRegistry
	MCPHandler http.Handler
}

// WireResult holds the assembled router and resolved deps for
// inspection by tests.
type WireResult struct {
	Handler    http.Handler
	AuthPool   *pgxpool.Pool
	ActionDeps *api.ActionDeps
	DBDeps     *api.DatabaseDeps
	// Canary is the fleet canary service (nil without a fleet and a
	// control database); the API server sweeps its interrupted runs.
	Canary *rollout.CanaryService
}

// apiRateLimit is the API rate limiter: per platform identity in a
// Databricks App, per client address elsewhere.
func apiRateLimit(p WireParams) func(http.Handler) http.Handler {
	rl := p.RateLimiter
	return func(next http.Handler) http.Handler {
		if p.DatabricksAppAuth != nil {
			return databricksAppRateLimitMiddleware(rl, *p.DatabricksAppAuth, next)
		}
		return rateLimitMiddleware(rl, next)
	}
}

// wireRouter assembles the HTTP handler from the given params.
// This is the testable core extracted from startAPIServer.
func wireRouter(p WireParams) (WireResult, error) {
	// Meta mode has an explicit canonical control pool. Monitored
	// databases are replaceable and must never own auth or sessions.
	var authPool *pgxpool.Pool
	if p.MetaState != nil && p.MetaState.Pool != nil {
		authPool = p.MetaState.Pool
	} else if p.FleetMgr != nil {
		authPool = p.FleetMgr.PoolForDatabase("all")
	}
	if authPool == nil {
		authPool = p.Pool
	}

	// ActionDeps: standalone vs fleet.
	var actionDeps *api.ActionDeps
	if p.Actions.Store != nil && p.Actions.Executor != nil {
		actionDeps = &api.ActionDeps{
			Store:    p.Actions.Store,
			Executor: p.Actions.Executor,
		}
	} else if p.FleetMgr != nil {
		actionDeps = &api.ActionDeps{
			Fleet: p.FleetMgr,
		}
	}

	// DatabaseDeps use an explicit meta control store, a standalone
	// store, or nil. YAML fleet mode has no stable control database.
	var dbDeps *api.DatabaseDeps
	if p.MetaState != nil && p.MetaState.Store != nil {
		metaState := p.MetaState
		dbDeps = &api.DatabaseDeps{
			Store:     metaState.Store,
			Fleet:     p.FleetMgr,
			Discovery: store.NewDiscoveryStore(metaState.Pool),
			ApplyCreate: func(
				ctx context.Context, input store.DatabaseInput, createdBy int,
			) (*store.DatabaseRecord, error) {
				if p.FleetMgr == nil {
					return nil, fmt.Errorf("fleet manager is unavailable")
				}
				return applyMetaDatabaseCreate(
					ctx, p.FleetMgr, metaState, input, createdBy,
				)
			},
			ApplyUpdate: func(
				ctx context.Context,
				id int,
				oldRec store.DatabaseRecord,
				input store.DatabaseInput,
			) (*store.DatabaseRecord, error) {
				if p.FleetMgr == nil {
					return nil, fmt.Errorf("fleet manager is unavailable")
				}
				return applyMetaDatabaseUpdate(
					ctx, p.FleetMgr, metaState, id, oldRec, input,
				)
			},
			ApplyDelete: func(
				ctx context.Context, id int, rec store.DatabaseRecord,
			) error {
				if p.FleetMgr == nil {
					return fmt.Errorf("fleet manager is unavailable")
				}
				return applyMetaDatabaseDelete(
					ctx, p.FleetMgr, metaState, id, rec,
				)
			},
		}
	} else if p.Pool != nil &&
		(p.Cfg == nil || !p.Cfg.IsFleet()) {
		dbDeps = &api.DatabaseDeps{
			Store: store.NewDatabaseStore(p.Pool, nil),
		}
	} else if p.FleetMgr != nil {
		// Keep lifecycle routes absent: every fleet pool is replaceable.
		dbDeps = nil
	}

	// Build middlewares. NewRouterFullRuntime applies this slice in reverse,
	// so the limiter must be first to run before auth performs a DB lookup.
	middlewares := make([]func(http.Handler) http.Handler, 0, 2)
	if p.RateLimiter != nil {
		middlewares = append(middlewares, apiRateLimit(p))
	}
	if p.DatabricksAppAuth != nil {
		middlewares = append(middlewares, api.DatabricksAppAuthMiddleware(authPool, *p.DatabricksAppAuth))
	} else {
		middlewares = append(middlewares, api.SessionAuthMiddleware(authPool))
	}

	specialistHandler, specialistAudit := specialistAPIDeps(p.Cfg, p.FleetMgr, authPool)
	autonomy := autonomyAPIDeps(p.FleetMgr, authPool)
	// Auth audit retains the client address independently of App identity buckets.
	api.SetClientIPResolver(clientIP)
	// X-Forwarded-Proto marks a session cookie Secure only from a trusted proxy.
	api.SetForwardedProtoTrust(fromTrustedProxy)
	router, err := api.NewRouterFullRuntime(
		p.FleetMgr, p.Cfg, authPool, actionDeps, dbDeps,
		p.LLMMgr, &api.RuntimeDeps{
			DatabricksAppAuth: p.DatabricksAppAuth != nil,
			ConfigController:  p.Config,
			ConfigBase:        p.ConfigBase,
			ConfigBaseLoader:  p.ConfigBaseLoader,
			LLMBudgets:        p.LLMBudgets,
			TelemetryStatus:   lakebaseTelemetryStatuses,
			TelemetryHistory:  lakebaseTelemetryHistory,
			LakebaseWaitLoad:  lakebaseWaitLoad, // market-01b
			// Expiry times and failure classes only; never tokens (databricks-06).
			LakebaseCredentials:  databricksauth.CredentialStatuses,
			LakebaseEndpointInfo: databricksauth.EndpointInfoFor,
			// Same key and policy as the runtime dispatchers (G7-B20/B21).
			NotificationSecretKey:         notificationSecretKey(authPool),
			NotificationPreviousSecretKey: notificationPreviousSecretKey(authPool),
			NotificationTargetPolicy:      notificationTargetPolicy(),
			CaseTicketSync:                true, // market-14
			SecretsPosture:                secretsPostureSource(authPool),
			DisableConfigWrites: p.Cfg != nil && p.Cfg.IsFleet() &&
				!p.Cfg.HasMetaDB(),
			MCPHandler: p.MCPHandler,
			MCPOAuth:   mcpOAuthValidator(p.Cfg, p.FleetMgr, authPool),
			Audit:      auditAPIDeps(p.Cfg, authPool),
			// evid-02c: keys the workload pack model tier's identifier hashing.
			WorkloadPseudonymKey: workloadPseudonymKey(authPool),
			Autonomy:             autonomy,
			Ask:                  askServices(),
			Specialist:           specialistHandler,
			// Look-alikes and leader status (fleet learning).
			FleetLearning: fleetLearningAPI{},
			// The request audit of the Postgres-specialist contract.
			SpecialistAudit: specialistAudit,
			// Agent environment labels (agent_envclass_wiring.go).
			AgentEnvironments: agentEnvironmentService(p.Cfg, p.FleetMgr, p.MetaState),
			// Kill switch, freeze and unfreeze (agent_kill_wiring.go).
			AgentKill: lateKillSwitch{},
			// An agent's activity (agent_broker_wiring.go).
			AgentActivity: agentActivityAPI{},
			// Agent grants and capability requests (agent_grants_wiring.go).
			AgentGrants: agentGrantAPI(p.Cfg, p.FleetMgr, p.MetaState),
			// tls_expiring in /ready details when the API serves TLS.
			TLSExpiring: apiTLSExpiringFunc(apiTLS),
			// GET /metrics on this listener (prometheus.serve_on_api).
			Metrics: apiMetricsRoute(p, authPool),
			// Where restore drill rows live (drill-01d).
			RestoreDrillResults: restoreDrillResults(p.Cfg, p.FleetMgr, p.MetaState),
		},
		middlewares...,
	)
	if err != nil {
		return WireResult{}, err
	}
	return WireResult{
		Handler:    router,
		AuthPool:   authPool,
		ActionDeps: actionDeps,
		DBDeps:     dbDeps,
		Canary:     autonomy.Canary,
	}, nil
}

func applyMetaDatabaseCreate(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state *metaDBState,
	input store.DatabaseInput,
	createdBy int,
) (*store.DatabaseRecord, error) {
	return applyMetaDatabaseCreateWith(ctx, mgr, state, input.Name,
		func(ctx context.Context) (int, error) {
			return state.Store.Create(ctx, input, createdBy)
		})
}

// applyMetaDatabaseCreateWith registers the member create inserts as name:
// the row is deleted again when its runtime cannot be published.
func applyMetaDatabaseCreateWith(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state *metaDBState,
	name string,
	create func(context.Context) (int, error),
) (*store.DatabaseRecord, error) {
	var id int
	var rec *store.DatabaseRecord
	var candidate *fleet.DatabaseInstance
	err := mgr.WithLifecycle(ctx, func(op *fleet.LifecycleMutation) (resultErr error) {
		if err := op.ValidateRegistration(name); err != nil {
			return err
		}
		id, resultErr = create(ctx)
		if resultErr != nil {
			return resultErr
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(
					resultErr,
					state.Store.Delete(context.WithoutCancel(ctx), id),
				)
			}
		}()
		rec, resultErr = state.Store.Get(ctx, id)
		if resultErr != nil {
			return resultErr
		}
		candidate, resultErr = prepareStoreDatabase(ctx, state, *rec)
		if resultErr != nil {
			return resultErr
		}
		if resultErr = healthCheckStoreDatabase(ctx, candidate); resultErr != nil {
			return resultErr
		}
		return op.PublishRegistration(candidate)
	})
	if err != nil {
		err = errors.Join(err, cleanupManagedCandidate(candidate))
		return nil, err
	}
	updateInstanceFindings(context.Background(), candidate)
	logInfo("meta-db", "db %q: initialized", candidate.Name)
	return rec, nil
}

func applyMetaDatabaseUpdate(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state *metaDBState,
	id int,
	oldRec store.DatabaseRecord,
	input store.DatabaseInput,
) (*store.DatabaseRecord, error) {
	prospective := updatedDatabaseRecord(oldRec, input)
	prepare := func(ctx context.Context) (*fleet.DatabaseInstance, error) {
		connStr, err := state.Store.GetUpdateConnectionString(ctx, id, input)
		if err != nil {
			return nil, err
		}
		actionDSN, err := state.Store.GetUpdateActionConnectionString(ctx, id, input)
		action, err := classifyMemberAction(actionDSN, err, prospective.ActionUsername, connStr)
		if err != nil {
			return nil, err
		}
		return prepareStoreDatabaseConnection(ctx, prospective, connStr, action)
	}
	persist := func(ctx context.Context) error {
		return state.Store.Update(ctx, id, input)
	}
	candidate, retired, err := replaceManagedDatabase(
		ctx, mgr, oldRec.Name, prospective.Name,
		prepare, healthCheckStoreDatabase, persist,
	)
	if err != nil {
		return nil, err
	}
	if err := fleet.ShutdownInstance(ctx, retired); err != nil {
		logWarn("meta-db",
			"db %q: replacement published; old runtime still draining: %v",
			candidate.Name, err)
	}
	// After the drain: the old runtime's in-flight work may still count.
	if oldRec.Name != candidate.Name && mgr.GetInstance(oldRec.Name) == nil {
		forgetRemovedDatabase(oldRec.Name)
	}
	updateInstanceFindings(context.Background(), candidate)
	logInfo("meta-db", "db %q: replaced", candidate.Name)
	if rec, readErr := state.Store.Get(ctx, id); readErr == nil {
		return rec, nil
	}
	return &prospective, nil
}

func applyMetaDatabaseDelete(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state *metaDBState,
	id int,
	rec store.DatabaseRecord,
) error {
	retired, err := deleteManagedDatabase(
		ctx, mgr, id,
		func(ctx context.Context) error { return state.Store.Delete(ctx, id) },
	)
	if err != nil {
		return err
	}
	if err := fleet.ShutdownInstance(context.WithoutCancel(ctx), retired); err != nil {
		logWarn("meta-db",
			"db %q: delete committed; runtime drain incomplete: %v",
			rec.Name, err)
	}
	// After the drain, unless the name was added back meanwhile.
	if mgr.GetInstance(rec.Name) == nil {
		forgetRemovedDatabase(rec.Name)
	}
	return nil
}

func deleteManagedDatabase(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	databaseID int,
	persist func(context.Context) error,
) (*fleet.DatabaseInstance, error) {
	var retired *fleet.DatabaseInstance
	err := mgr.WithLifecycle(ctx, func(op *fleet.LifecycleMutation) error {
		retired = op.CurrentByDatabaseID(databaseID)
		if err := persist(ctx); err != nil {
			return err
		}
		return op.DetachInstance(retired)
	})
	return retired, err
}

func replaceManagedDatabase(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	oldName, newName string,
	prepare func(context.Context) (*fleet.DatabaseInstance, error),
	health func(context.Context, *fleet.DatabaseInstance) error,
	persist func(context.Context) error,
) (*fleet.DatabaseInstance, *fleet.DatabaseInstance, error) {
	var candidate, retired *fleet.DatabaseInstance
	published := false
	err := mgr.WithLifecycle(ctx, func(op *fleet.LifecycleMutation) error {
		var err error
		retired, err = op.ValidateReplacement(oldName, newName, nil)
		if err != nil {
			return err
		}
		candidate, err = prepare(ctx)
		if err != nil {
			return err
		}
		if err = health(ctx, candidate); err != nil {
			return err
		}
		if err = persist(ctx); err != nil {
			return err
		}
		if err = op.PublishReplacement(oldName, retired, candidate); err != nil {
			return err
		}
		published = true
		return nil
	})
	if err != nil && candidate != nil && !published {
		err = errors.Join(err, cleanupManagedCandidate(candidate))
	}
	return candidate, retired, err
}

// forgetRemovedDatabase releases what the process keeps per database name
// once that name has left it (deleted, renamed or removed by a reload):
// the LLM budget share returns to the remaining databases (G5-B06), its
// Sage SRE probe queue series leave /metrics (sre-06), and the
// process-wide /metrics counters drop its series (observability-07), the
// LLM health counters included (observability-05), its workload
// evidence pack series (evid-02a), its hourly query rollup series
// (evid-01a) and its measured-outcome share (ux-02a).
func forgetRemovedDatabase(name string) {
	if fleetLLMBudget != nil {
		fleetLLMBudget.Unregister(name)
	}
	probes.ForgetQueueStats(name)
	executor.PruneCounts(name)
	shadow.Prune(name)
	llmHealth.prune(name)
	ask.ForgetDatabase(name)
	selfbudget.Process().ForgetDatabase(name)
	workpack.ForgetDatabase(name)
	querystore.ForgetRollupDatabase(name)
	outcomes.ForgetDatabase(name)
}

func cleanupManagedCandidate(candidate *fleet.DatabaseInstance) error {
	if candidate == nil {
		return nil
	}
	return fleet.ShutdownInstance(context.Background(), candidate)
}

func updatedDatabaseRecord(
	old store.DatabaseRecord, input store.DatabaseInput,
) store.DatabaseRecord {
	old.Name = input.Name
	old.Host = input.Host
	old.Port = input.Port
	old.DatabaseName = input.DatabaseName
	old.Username = input.Username
	old.SSLMode = input.SSLMode
	old.MaxConnections = input.MaxConnections
	old.Tags = input.Tags
	old.TrustLevel = input.TrustLevel
	old.ExecutionMode = input.ExecutionMode
	if input.Action != nil {
		old.ActionUsername = input.Action.Username
	}
	return old
}
