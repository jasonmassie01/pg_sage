package testdb

import (
	"context"

	"github.com/pg-sage/sidecar/internal/clusterlock"
)

// AgentRolesLock is the LockCluster name every test that creates agent
// roles (sage_agent_*, sage_agentb_*) holds, together with the posture tests
// that assume none exists: the roles are cluster-wide.
const AgentRolesLock = "agent_roles"

// LockCluster serializes fixtures that change cluster-wide state (WAL
// volume, replication slots, the archiver) across test packages, which
// run in parallel on one server. It is clusterlock.Lock: the lock is
// taken in the server's maintenance database (advisory locks are per
// database), and release ends the session and with it the lock.
func LockCluster(ctx context.Context, dsn, name string) (func(), error) {
	return clusterlock.Lock(ctx, dsn, name)
}
