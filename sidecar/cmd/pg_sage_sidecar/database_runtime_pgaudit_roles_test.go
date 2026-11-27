package main

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// No concurrency tests here: the source only reads its arguments; the role
// cache it feeds is tested under concurrency in internal/pgaudit.

// The pgaudit role resolver reads agent governance's control database, found
// at each read (a control database registered later is still found), and
// no control database is no querier at all, never a typed nil pool.
func TestPGAuditRoleSourceFollowsTheControlDatabase(t *testing.T) {
	mgr, pools := managerWith(t, "app", "governance")
	c := &config.Config{Agents: config.AgentsConfig{ControlDatabase: "governance"}}
	var current *fleet.DatabaseManager
	var meta *metaDBState
	source := pgauditRoleSource(func() (*config.Config, *fleet.DatabaseManager,
		*metaDBState) {
		return c, current, meta
	})
	if q := source(); q != nil {
		t.Fatalf("no fleet yet: querier %v, want nil", q)
	}
	current = mgr
	if q := source(); q != pools["governance"] {
		t.Fatalf("control database = %v, want the governance pool", q)
	}
	meta = &metaDBState{Pool: lazyPool(t, "meta")}
	if q := source(); q != meta.Pool {
		t.Fatalf("meta mode: querier %v, want the meta pool", q)
	}
	none := pgauditRoleSource(func() (*config.Config, *fleet.DatabaseManager,
		*metaDBState) {
		return nil, nil, nil
	})
	if q := none(); q != nil {
		t.Fatalf("no config: querier %v, want nil", q)
	}
}

// The process keeps one role cache for every database's correlator.
func TestPGAuditRoleCacheIsShared(t *testing.T) {
	a, b := pgauditRoleCache(), pgauditRoleCache()
	if a == nil || a != b {
		t.Fatalf("role caches %p and %p, want one shared cache", a, b)
	}
}

// stderrOf runs fn and returns what it wrote to stderr (where the sidecar
// logs).
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := swapStderr(w)
	fn()
	swapStderr(saved)
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// A correlator that reads the roles before the control database has
// registered logs nothing and leaves governance's one posture-only message
// for governance, which still reports it afterwards.
func TestPGAuditRoleSourceKeepsGovernancePostureMessage(t *testing.T) {
	agentPostureOnce = sync.Once{}
	t.Cleanup(func() { agentPostureOnce = sync.Once{} })
	mgr, _ := managerWith(t, "app")
	c := &config.Config{Agents: config.AgentsConfig{ControlDatabase: "governance"}}
	source := pgauditRoleSource(func() (*config.Config, *fleet.DatabaseManager,
		*metaDBState) {
		return c, mgr, nil
	})
	if out := stderrOf(t, func() {
		if q := source(); q != nil {
			t.Errorf("control database not registered: querier %v, want nil", q)
		}
	}); out != "" {
		t.Fatalf("the pgaudit role source logged %q", out)
	}
	out := stderrOf(t, func() { _ = governanceControlPool(c, mgr, nil) })
	if !strings.Contains(out, "posture checks only") {
		t.Fatalf("governance's posture message was spent: got %q", out)
	}
}
