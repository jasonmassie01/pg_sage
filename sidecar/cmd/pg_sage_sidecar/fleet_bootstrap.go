package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sagepool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
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
	rt, err := prepareFleetRuntimeFrom(dbCfg, b.controlPool, openFleetPoolAtStartup)
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

// prepareFleetRuntime connects one YAML database in a single attempt and
// builds its runtime without publishing it, for reloads and provider
// re-probes: an operator-visible reload reports an unreachable database at
// once instead of waiting out startup backoff. control is the fleet's control
// pool; nil makes the database its own control.
func prepareFleetRuntime(
	dbCfg config.DatabaseConfig, control *pgxpool.Pool,
) (*databaseRuntime, error) {
	return prepareFleetRuntimeFrom(dbCfg, control, openFleetPool)
}

func prepareFleetRuntimeFrom(
	dbCfg config.DatabaseConfig, control *pgxpool.Pool,
	open func(config.DatabaseConfig) (*pgxpool.Pool, error),
) (*databaseRuntime, error) {
	logInfo("fleet", "connecting to database %q", dbCfg.Name)
	dbPool, err := open(dbCfg)
	if err != nil {
		logError("fleet", "db %q: %v", dbCfg.Name, err)
		return nil, err
	}
	logInfo("fleet", "db %q: connected", dbCfg.Name)
	actionPool, err := openActionPool(dbCfg.ConnString(), dbCfg)
	if err != nil {
		dbPool.Close()
		logError("fleet", "%v", err)
		return nil, err
	}
	if control == nil {
		control = dbPool
	}
	rt, err := buildDatabaseRuntime(context.Background(), databaseRuntimeSpec{
		Scope: "fleet", Name: dbCfg.Name, Config: dbCfg, Pool: dbPool,
		ActionPool: actionPool, ControlPool: control,
		ExecMode: resolveStaticFleetExecMode(dbCfg), Parent: shutdownCtx,
	})
	if err != nil {
		closeActionPool(actionPool)
		dbPool.Close()
		logError("fleet", "db %q: %v", dbCfg.Name, err)
		return nil, err
	}
	return rt, nil
}

// openFleetPool connects a YAML database with the sidecar's pool settings in
// one attempt.
func openFleetPool(dbCfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	return openFleetPoolAttempts(dbCfg, 1)
}

// openFleetPoolAtStartup retries transient endpoint and connection failures
// like single-database startup does: a startup failure leaves the database
// registered as failed until the next reload.
func openFleetPoolAtStartup(dbCfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	return openFleetPoolAttempts(dbCfg, startupAttempts)
}

// openFleetPoolAttempts resolves, connects and pings a YAML database;
// shutdown interrupts the retries.
func openFleetPoolAttempts(dbCfg config.DatabaseConfig, attempts int) (*pgxpool.Pool, error) {
	base, err := sagepool.ParseConfig(dbCfg.ConnString())
	if err != nil {
		return nil, fmt.Errorf("invalid DSN: %w", err)
	}
	base.MaxConns = int32(max(dbCfg.MaxConnections, 2))
	ctx := shutdownCtx
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := retryStartup(ctx, "fleet db "+dbCfg.Name, attempts,
		func(ctx context.Context) (*pgxpool.Pool, error) {
			return openStartupPool(ctx, base, func(poolCfg *pgxpool.Config) {
				poolCfg.MinConns = 1
				poolCfg.MaxConnLifetime = 30 * time.Minute
				poolCfg.MaxConnIdleTime = 5 * time.Minute
				// A stable application_name lets the analyzer and executor
				// recognize every sidecar backend; tagged statements keep
				// pg_sage visible in pg_stat_statements and out of its own
				// analysis.
				selfmonitor.ConfigurePool(poolCfg)
			})
		})
	warnLakebaseScaleToZero("fleet db "+dbCfg.Name, p, monitoredCollectorInterval())
	return p, err
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
		Status: &fleet.InstanceStatus{Error: err.Error(), LastSeen: time.Now(),
			// Never read, so unknown rather than zero (cases-11).
			IncidentsCritical: fleet.IncidentsUnknown,
			IncidentsWarning:  fleet.IncidentsUnknown},
	}
}
