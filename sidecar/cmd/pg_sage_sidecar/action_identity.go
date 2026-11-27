package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// actionPoolProbeTimeout bounds the startup reachability check.
const actionPoolProbeTimeout = 10 * time.Second

// openActionPool builds the separate action identity's pool when the
// database sets action_dsn; it returns nil without one (single identity).
// observation is the DSN the observation pool connected with: the action
// DSN must reach the same host, port and database as another role. The
// pool connects lazily (MinConns 0): an action role that is down leaves
// observation running and fails each change with "action identity
// unavailable" instead.
func openActionPool(observation string, db config.DatabaseConfig) (*pgxpool.Pool, error) {
	if db.ActionDSN == "" {
		return nil, nil
	}
	if err := config.ValidateActionDSN(observation, db.ActionDSN); err != nil {
		return nil, fmt.Errorf("db %q: %w", db.Name, err)
	}
	pc, err := pgxpool.ParseConfig(db.ActionDSN)
	if err != nil {
		return nil, fmt.Errorf("db %q: action_dsn does not parse", db.Name)
	}
	// Connections open only while a change or a signal runs (MinConns 0)
	// and close after five idle minutes: on Lakebase and Cloud SQL the
	// second role costs connections only while it acts.
	pc.MaxConns, pc.MinConns = executor.ActionPoolMaxConns(), 0
	pc.MaxConnLifetime = 30 * time.Minute
	pc.MaxConnIdleTime = 5 * time.Minute
	// The executor and the backend guard recognize pg_sage's sessions by
	// application_name, so the action role is tagged like every pool.
	selfmonitor.ConfigurePool(pc)
	p, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, fmt.Errorf("db %q: action identity pool: %w", db.Name, err)
	}
	probeActionPool(db.Name, p)
	return p, nil
}

// probeActionPool reports at startup whether the action identity answers;
// a failure is a warning, never fatal, since observation needs no action
// role.
func probeActionPool(name string, p *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), actionPoolProbeTimeout)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		logWarn("startup", "db %q: action identity (action_dsn) does not answer: %v; "+
			"observation continues and every change fails until it does", name, err)
		return
	}
	logInfo("startup", "db %q: action identity %q connected; changes and backend "+
		"signals run as it", name, p.Config().ConnConfig.User)
}

// actionRole is the action identity's role name; "" for a single identity.
func actionRole(p *pgxpool.Pool) string {
	if p == nil {
		return ""
	}
	return p.Config().ConnConfig.User
}

func closeActionPool(p *pgxpool.Pool) {
	if p != nil {
		p.Close()
	}
}
