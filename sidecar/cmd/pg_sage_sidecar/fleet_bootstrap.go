package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// fleetBootstrap tracks YAML fleet startup: the control database is the
// first one whose runtime starts, and the admin user is created there.
type fleetBootstrap struct {
	controlPool *pgxpool.Pool
	// controlName is the control database's fleet name. Auth, sessions,
	// standing policy and notification rules live there, so a reload may
	// not remove or reconnect it.
	controlName string
	initialized int
}

// start connects one YAML database and builds its runtime. A database that
// cannot connect or be prepared is registered as failed so the dashboard
// shows why.
func (b *fleetBootstrap) start(dbCfg config.DatabaseConfig) {
	rt, err := prepareFleetRuntime(dbCfg, b.controlPool)
	if err != nil {
		registerFailedFleetDatabase(dbCfg, err)
		return
	}
	if b.controlPool == nil {
		// The admin lands where logins are validated. Gating on config
		// index 0 locked the dashboard when that database was down (LIVE-03).
		if err := bootstrapAdminIfEmpty(context.Background(), rt.spec.Pool); err != nil {
			logWarn("fleet", "admin bootstrap: %v", err)
		}
		b.controlPool, b.controlName = rt.spec.Pool, dbCfg.Name
	}
	rt.publish(fleetMgr)
	b.initialized++
}

// prepareFleetRuntime connects one YAML database and builds its runtime
// without publishing it, for startup and for reloads alike. control is the
// fleet's control pool; nil makes the database its own control.
func prepareFleetRuntime(
	dbCfg config.DatabaseConfig, control *pgxpool.Pool,
) (*databaseRuntime, error) {
	logInfo("fleet", "connecting to database %q", dbCfg.Name)
	dbPool, err := openFleetPool(dbCfg)
	if err != nil {
		logError("fleet", "db %q: %v", dbCfg.Name, err)
		return nil, err
	}
	logInfo("fleet", "db %q: connected", dbCfg.Name)
	if control == nil {
		control = dbPool
	}
	rt, err := buildDatabaseRuntime(context.Background(), databaseRuntimeSpec{
		Scope: "fleet", Name: dbCfg.Name, Config: dbCfg, Pool: dbPool,
		ControlPool: control, ExecMode: resolveStaticFleetExecMode(dbCfg),
		Parent: shutdownCtx,
	})
	if err != nil {
		dbPool.Close()
		logError("fleet", "db %q: %v", dbCfg.Name, err)
		return nil, err
	}
	return rt, nil
}

// openFleetPool connects a YAML database with the sidecar's pool settings.
func openFleetPool(dbCfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dbCfg.ConnString())
	if err != nil {
		return nil, fmt.Errorf("invalid DSN: %w", err)
	}
	poolCfg.MaxConns = int32(max(dbCfg.MaxConnections, 2))
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	// A stable application_name lets the analyzer and executor recognize
	// every sidecar backend.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "pg_sage"
	// Keep pg_sage's own monitoring queries out of pg_stat_statements
	// (best-effort; the /* pg_sage */ tag filter is the fallback).
	poolCfg.AfterConnect = silenceSelfStats
	dbPool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dbPool.Ping(ctx); err != nil {
		dbPool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return dbPool, nil
}

func registerFailedFleetDatabase(dbCfg config.DatabaseConfig, err error) {
	fleetMgr.RegisterInstance(failedFleetInstance(dbCfg, err))
}

// failedFleetInstance is the dashboard placeholder of a database that
// could not be connected or prepared.
func failedFleetInstance(dbCfg config.DatabaseConfig, err error) *fleet.DatabaseInstance {
	return &fleet.DatabaseInstance{
		Name:   dbCfg.Name,
		Config: dbCfg,
		Status: &fleet.InstanceStatus{Error: err.Error(), LastSeen: time.Now()},
	}
}
