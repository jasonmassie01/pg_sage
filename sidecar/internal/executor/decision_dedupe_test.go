package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Dogfood lifeos: the standing gate wrote a new sage.decision row on every
// evaluation of every candidate, every cycle (with the schema guard, about
// 41,000 rows an hour). Repeated non-execute verdicts now update one row,
// and the cheap read-only skips run before the gate.

func TestLedgerInputFingerprintsOnlyNonExecuteVerdicts(t *testing.T) {
	id := 42
	request := policy.ActionRequest{Feature: "index", SQL: "CREATE INDEX CONCURRENTLY i ON t (c)",
		TargetObjs: []string{"public.t"}}
	for _, verdict := range []policy.Verdict{policy.VerdictPark, policy.VerdictQueueApproval,
		policy.VerdictObserveOnly, policy.VerdictBlocked} {
		input := ledgerInput(&id, 3, request, policy.Decision{Verdict: verdict,
			Reason: policy.ReasonPolicyUnavailable})
		if input.Fingerprint == "" || input.Fingerprint != ledger.DecisionFingerprint(input) {
			t.Errorf("%s: fingerprint %q, want the ledger fingerprint", verdict, input.Fingerprint)
		}
	}
	execute := ledgerInput(&id, 3, request, policy.Decision{Verdict: policy.VerdictExecute})
	if execute.Fingerprint != "" {
		t.Fatalf("execute fingerprint = %q, want none (it backs its own action)",
			execute.Fingerprint)
	}
	first := ledgerInput(&id, 3, request, policy.Decision{Verdict: policy.VerdictPark})
	again := ledgerInput(&id, 3, request, policy.Decision{Verdict: policy.VerdictPark})
	if first.Fingerprint != again.Fingerprint || first.EvidenceID == again.EvidenceID {
		t.Fatal("repeat evaluations must share a fingerprint, not an evidence ID")
	}
	newer := ledgerInput(&id, 4, request, policy.Decision{Verdict: policy.VerdictPark})
	if newer.Fingerprint == first.Fingerprint {
		t.Fatal("a new policy version kept the fingerprint")
	}
}

// countingGate counts authorizations and answers with one verdict.
type countingGate struct {
	mu    sync.Mutex
	calls int
	inner fixedGate
}

func (g *countingGate) Authorize(ctx context.Context, r policy.ActionRequest) policy.Decision {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return g.inner.Authorize(ctx, r)
}

func (g *countingGate) Explain(ctx context.Context, r policy.ActionRequest) policy.Decision {
	return g.inner.Explain(ctx, r)
}

func (g *countingGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestCheapSkipsRunBeforeTheGate(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictPark)
	gate := &countingGate{inner: fixedGate{verdict: policy.VerdictPark}}
	fx.exec.WithPolicyGate(gate)

	missing := fx.f
	missing.Category = "rec_cycle_missing"
	fx.exec.processFinding(fx.ctx, missing, false, nil)
	if gate.count() != 0 {
		t.Fatalf("finding without an open row consulted the gate %d times", gate.count())
	}
	fx.exec.recentMu.Lock()
	fx.exec.recentActions[fx.f.ObjectIdentifier] = time.Now()
	fx.exec.recentMu.Unlock()
	fx.exec.processFinding(fx.ctx, fx.f, false, nil)
	if gate.count() != 0 {
		t.Fatalf("cascade cooldown consulted the gate %d times", gate.count())
	}
	fx.exec.recentMu.Lock()
	delete(fx.exec.recentActions, fx.f.ObjectIdentifier)
	fx.exec.recentMu.Unlock()
	fx.exec.processFinding(fx.ctx, fx.f, false, nil)
	if gate.count() != 1 {
		t.Fatalf("an eligible finding consulted the gate %d times, want 1", gate.count())
	}
}

func decisionsFor(t *testing.T, fx *recFixture) (rows, repeats int, verdict string) {
	t.Helper()
	target := fmt.Sprintf(`["%s"]`, fx.f.ObjectIdentifier)
	err := fx.pool.QueryRow(fx.ctx, `SELECT count(*), COALESCE(sum(repeat_count), 0),
		COALESCE(max(verdict), '') FROM sage.decision WHERE target_objects = $1::jsonb`,
		target).Scan(&rows, &repeats, &verdict)
	if err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	return rows, repeats, verdict
}

// Rows written per cycle on the gate path: three cycles over one candidate
// the real standing gate withholds write one row, seen three times.
func TestRepeatedWithheldCandidateWritesOneDecisionRow(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	fx.propose(t)
	databaseID := 880000 + int(time.Now().UnixNano()%100000)
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(),
			"DELETE FROM sage.policy WHERE database_id=$1", databaseID)
		_, _ = fx.pool.Exec(context.Background(),
			"DELETE FROM sage.decision WHERE database_id=$1", databaseID)
	})
	if err := fx.exec.EnableStandingPolicy(fx.ctx, "unattended", &databaseID); err != nil {
		t.Fatalf("EnableStandingPolicy: %v", err)
	}
	fx.exec.WithEmergencyStopCheck(func(context.Context) bool { return true })
	for cycle := 1; cycle <= 3; cycle++ {
		fx.exec.RunCycle(fx.ctx, false)
		rows, repeats, verdict := decisionsFor(t, fx)
		if rows != 1 || repeats != cycle || verdict == "execute" || verdict == "" {
			t.Fatalf("after cycle %d: rows=%d repeats=%d verdict=%q, want 1 withheld "+
				"row seen %d times", cycle, rows, repeats, verdict, cycle)
		}
	}
	if fx.actionRows(t) != 0 {
		t.Fatal("a withheld candidate was executed")
	}
}
