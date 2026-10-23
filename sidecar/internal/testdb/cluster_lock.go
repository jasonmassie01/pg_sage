package testdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AgentRolesLock is the LockCluster name every test that creates agent
// roles (sage_agent_*, sage_agentb_*) holds, together with the posture tests
// that assume none exists: the roles are cluster-wide.
const AgentRolesLock = "agent_roles"

// LockCluster serializes fixtures that change cluster-wide state (WAL
// volume, replication slots, the archiver) across test packages, which
// run in parallel on one server. Package fixtures use separate
// databases, and advisory locks are per database, so the lock is taken
// in the server's maintenance database. release ends the session and
// with it the lock.
func LockCluster(ctx context.Context, dsn, name string) (func(), error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect maintenance database: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))",
		"pg_sage_cluster_fixture:"+name); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("cluster fixture lock %q: %w", name, err)
	}
	return func() { _ = conn.Close(context.Background()) }, nil
}
