//go:build perfgate

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Agent posture in the performance gate (G0-04): its statements run in
// the warm-up phase (the first look and the analyzer's first posture run)
// under gates A and D, and a run the gate triggers in the steady phase, by
// changing the catalog posture reads, stays inside gates B, C and G.

// perfAgentRole is a registered agent role (Guard naming, spec section 6.6), so
// the detectors that read agent roles run their statements too.
const perfAgentRole = "sage_agentb_perfgate22"

// setupPerfPosture creates the agent role before the runtime starts; it is
// dropped when the test ends.
func setupPerfPosture(t *testing.T, ctx context.Context, harness *pgxpool.Pool) {
	t.Helper()
	testdb.LockAgentRoles(t) // agent roles are cluster-wide
	createPerfRole(t, ctx, harness, perfAgentRole+" LOGIN", perfAgentRole)
}

func createPerfRole(t *testing.T, ctx context.Context, harness *pgxpool.Pool, spec,
	name string) {
	t.Helper()
	tag := "/* " + perfgate.HarnessTag + " */ "
	if _, err := harness.Exec(ctx, tag+"DROP ROLE IF EXISTS "+name); err != nil {
		t.Fatalf("drop a leftover %s: %v", name, err)
	}
	if _, err := harness.Exec(ctx, tag+"CREATE ROLE "+spec); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := harness.Exec(context.Background(), tag+"DROP ROLE IF EXISTS "+
			name); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
}

// triggerPerfPosture creates a role, which changes the catalog fingerprint
// the posture monitor watches, so its next cycle runs every detector. The
// role is dropped when the test ends.
func triggerPerfPosture(t *testing.T, ctx context.Context, harness *pgxpool.Pool) {
	t.Helper()
	role := fmt.Sprintf("perfgate_posture_%d", os.Getpid())
	createPerfRole(t, ctx, harness, role+" NOLOGIN", role)
}

// requirePostureMeasured fails unless every registered detector's
// statement ran in the phase, so the gates measured posture there.
func requirePostureMeasured(t *testing.T, phase perfgate.Phase) {
	t.Helper()
	for _, d := range agentposture.Default().Detectors() {
		marker := agentposture.Section + " " + d.Spec().ID + " "
		if !slicesContainsQuery(phase.Statements, marker) {
			t.Errorf("%s phase: no %s statement in pg_stat_statements; the gate did "+
				"not measure posture", phase.Name, d.Spec().ID)
		}
	}
}

func slicesContainsQuery(stmts []perfgate.Statement, marker string) bool {
	for _, s := range stmts {
		if strings.Contains(s.Query, marker) {
			return true
		}
	}
	return false
}
