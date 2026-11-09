package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-02: a withheld verdict is one row per episode. The standing
// gate's execute verdict ends the candidate's open withheld rows, so a park
// after the execute starts a new episode instead of counting weeks of
// repeats on the row of the first one.

type episodeDecision struct {
	ID          int64
	Verdict     string
	Fingerprint *string
	Repeats     int
	Resolved    bool
}

func episodeDecisions(t *testing.T, exec *Executor, target string) []episodeDecision {
	t.Helper()
	rows, err := exec.pool.Query(context.Background(), `SELECT id, verdict, fingerprint,
		repeat_count, resolved_at IS NOT NULL FROM sage.decision
		WHERE target_objects = $1::jsonb ORDER BY id`, `["`+target+`"]`)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	defer rows.Close()
	var out []episodeDecision
	for rows.Next() {
		var d episodeDecision
		if err := rows.Scan(&d.ID, &d.Verdict, &d.Fingerprint, &d.Repeats,
			&d.Resolved); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

var (
	episodePark = policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonOutsideMaintenanceWindow, RiskTier: policy.RiskModerate}
	episodeExecute = policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAuthorized, RiskTier: policy.RiskModerate,
		BudgetKind: policy.BudgetPerformance}
)

// recordInBudget records decision as the gate does for a budget-spending
// request: inside the budget transaction, committed when commit is true.
func recordInBudget(
	t *testing.T, exec *Executor, req policy.ActionRequest, decision policy.Decision,
) (int64, error) {
	t.Helper()
	ctx := context.Background()
	lockedCtx, done, err := exec.serializeBudget(ctx, req)
	if err != nil {
		t.Fatalf("serializeBudget: %v", err)
	}
	_, id, err := exec.recordStandingDecisionAt(lockedCtx, 1, req, decision)
	if endErr := done(err == nil); endErr != nil && err == nil {
		t.Fatalf("end the budget transaction: %v", endErr)
	}
	return id, err
}

func TestExecuteEndsTheParkEpisode(t *testing.T) {
	pool, _ := isolatedSageDB(t)
	exec := budgetExecutor(t, pool, lifeosLegacyPolicy())
	target := fmt.Sprintf("public.episode_%d", time.Now().UnixNano())
	req := findingRequest(createIndexFinding(target), false)
	for range 3 {
		if _, err := recordInBudget(t, exec, req, episodePark); err != nil {
			t.Fatalf("park: %v", err)
		}
	}
	if _, err := recordInBudget(t, exec, req, episodeExecute); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := recordInBudget(t, exec, req, episodePark); err != nil {
		t.Fatalf("park after the execute: %v", err)
	}
	rows := episodeDecisions(t, exec, createIndexFinding(target).ObjectIdentifier)
	if len(rows) != 3 {
		t.Fatalf("decisions = %+v, want park episode, execute, new park", rows)
	}
	first, executed, next := rows[0], rows[1], rows[2]
	if first.Verdict != "parked" || first.Repeats != 3 || !first.Resolved {
		t.Fatalf("first park = %+v, want 3 repeats, resolved", first)
	}
	if executed.Verdict != "execute" || executed.Fingerprint != nil {
		t.Fatalf("execute = %+v, want an execute row without a fingerprint", executed)
	}
	if next.Verdict != "parked" || next.Repeats != 1 || next.Resolved ||
		next.Fingerprint == nil || first.Fingerprint == nil ||
		*next.Fingerprint != *first.Fingerprint {
		t.Fatalf("new park = %+v, want a fresh open row of the same fingerprint", next)
	}
}

// Error propagation: a resolve that cannot run inside the budget
// transaction fails the execute record, and nothing of it commits.
func TestResolveFailureFailsTheBudgetRecord(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := budgetExecutor(t, pool, lifeosLegacyPolicy())
	target := fmt.Sprintf("public.episode_lock_%d", time.Now().UnixNano())
	req := findingRequest(createIndexFinding(target), false)
	parkID, err := recordInBudget(t, exec, req, episodePark)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `SELECT 1 FROM sage.decision WHERE id = $1 FOR UPDATE`,
		parkID); err != nil {
		t.Fatalf("hold the park's row lock: %v", err)
	}
	lockedCtx, done, err := exec.serializeBudget(ctx, req)
	if err != nil {
		t.Fatalf("serializeBudget: %v", err)
	}
	t.Cleanup(func() { _ = done(false) })
	tx, _ := budgetTx(lockedCtx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatal(err)
	}
	_, _, err = exec.recordStandingDecisionAt(lockedCtx, 1, req, episodeExecute)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("execute record = %v, want the resolve's lock_not_available", err)
	}
	if err := done(false); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rows := episodeDecisions(t, exec, createIndexFinding(target).ObjectIdentifier)
	if len(rows) != 1 || rows[0].ID != parkID || rows[0].Resolved {
		t.Fatalf("decisions = %+v, want only the open park %d", rows, parkID)
	}
}

// The candidate key ignores the verdict and the intent, so an operator's
// approved execute ends an autonomous park of the same change.
func TestLedgerInputCandidateKeySpansVerdictsAndIntent(t *testing.T) {
	f := createIndexFinding("public.episode_key")
	park := ledgerInputAt(nil, 1, findingRequest(f, false), episodePark)
	approved := findingRequest(f, false)
	approved.OperatorApproved = true
	execute := ledgerInputAt(nil, 2, approved, episodeExecute)
	if park.Intent == execute.Intent {
		t.Fatalf("intents %q and %q: the test needs them to differ", park.Intent,
			execute.Intent)
	}
	if ledger.CandidateKey(park) != ledger.CandidateKey(execute) {
		t.Fatal("an operator execute and an autonomous park have different candidate keys")
	}
}
