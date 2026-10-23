package testdb

import (
	"context"
	"os"
	"sync"
	"testing"
)

// agentRoleLocks are the tests holding AgentRolesLock: a test that builds
// two agent-role fixtures takes it once (a second session would wait for
// the first forever).
var agentRoleLocks sync.Map // testing.TB -> struct{}

// LockAgentRoles holds AgentRolesLock on the designated test server for
// the rest of the test. Every test that creates agent-named roles
// (sage_agent_*, sage_agentb_*) takes it: the roles are cluster-wide, and
// the posture tests and a fleet kill assume no other test's roles exist.
// It is re-entrant within one test.
func LockAgentRoles(t testing.TB) {
	t.Helper()
	LockAgentRolesOn(t, os.Getenv(EnvName))
}

// LockAgentRolesOn is LockAgentRoles on the server of dsn.
func LockAgentRolesOn(t testing.TB, dsn string) {
	t.Helper()
	if _, held := agentRoleLocks.LoadOrStore(t, struct{}{}); held {
		return
	}
	release, err := LockCluster(context.Background(), dsn, AgentRolesLock)
	if err != nil {
		agentRoleLocks.Delete(t)
		t.Fatalf("agent roles lock: %v", err)
	}
	t.Cleanup(func() {
		release()
		agentRoleLocks.Delete(t)
	})
}
