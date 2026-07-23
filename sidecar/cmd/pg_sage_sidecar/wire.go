package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// WireParams captures all inputs for building the API router.
// Each field corresponds to a global variable or startup artifact
// that startAPIServer previously read directly.
type WireParams struct {
	Cfg       *config.Config
	Pool      *pgxpool.Pool // global pool (standalone or meta-db)
	FleetMgr  *fleet.DatabaseManager
	LLMMgr    *llm.Manager
	MetaState *metaDBState
	Actions   struct {
		Store    *store.ActionStore
		Executor *executor.Executor
	}
	RateLimiter *RateLimiter
	Config      *config.ConfigController
	ConfigBase  *config.Config
	MCPHandler  http.Handler
}

// WireResult holds the assembled router and resolved deps for
// inspection by tests.
type WireResult struct {
	Handler    http.Handler
	AuthPool   *pgxpool.Pool
	ActionDeps *api.ActionDeps
	DBDeps     *api.DatabaseDeps
}

// wireRouter assembles the HTTP handler from the given params.
// This is the testable core extracted from startAPIServer.
func wireRouter(p WireParams) WireResult {
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
			Store: metaState.Store,
			Fleet: p.FleetMgr,
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
		rl := p.RateLimiter
		middlewares = append(middlewares,
			func(next http.Handler) http.Handler {
				return rateLimitMiddleware(rl, next)
			})
	}
	middlewares = append(middlewares, api.SessionAuthMiddleware(authPool))

	router := api.NewRouterFullRuntime(
		p.FleetMgr, p.Cfg, authPool, actionDeps, dbDeps,
		p.LLMMgr, &api.RuntimeDeps{
			ConfigController: p.Config,
			ConfigBase:       p.ConfigBase,
			DisableConfigWrites: p.Cfg != nil && p.Cfg.IsFleet() &&
				!p.Cfg.HasMetaDB(),
			MCPHandler: p.MCPHandler,
		},
		middlewares...,
	)

	return WireResult{
		Handler:    router,
		AuthPool:   authPool,
		ActionDeps: actionDeps,
		DBDeps:     dbDeps,
	}
}

func applyMetaDatabaseCreate(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state *metaDBState,
	input store.DatabaseInput,
	createdBy int,
) (*store.DatabaseRecord, error) {
	var id int
	var rec *store.DatabaseRecord
	var candidate *fleet.DatabaseInstance
	err := mgr.WithLifecycle(ctx, func(op *fleet.LifecycleMutation) (resultErr error) {
		if err := op.ValidateRegistration(input.Name); err != nil {
			return err
		}
		id, resultErr = state.Store.Create(ctx, input, createdBy)
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
		return prepareStoreDatabaseConnection(ctx, prospective, connStr)
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
	return old
}
