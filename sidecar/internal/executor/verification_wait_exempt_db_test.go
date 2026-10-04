package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Exemptions, the operator override, the hard-deadline release, concurrent
// proposals, the metrics and the lookup's plan, on real Postgres.

// inFlightMemoriesIndex records lifeos action 6410 being verified.
func inFlightMemoriesIndex(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (
	int64, analyzer.Finding) {
	t.Helper()
	created := memoriesIndexFinding("idx_memories_live_status_type_quality", lifeos6410Where)
	id := recordInFlight(t, pool, ctx, created.RecommendedSQL, created.RollbackSQL,
		"monitoring", 10*time.Minute)
	pendingOutcome(t, pool, ctx, id, "index_create")
	return id, created
}

func TestOneChange_RollbackOfTheInFlightActionIsExempt(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	_, created := inFlightMemoriesIndex(t, pool, ctx)
	if !exec.standingRollbackAuthorizer(created)(ctx, created.RollbackSQL) {
		t.Fatal("the rollback of the in-flight action was not authorized")
	}
	// The same statement as a fresh self-initiated change waits.
	fresh := created
	fresh.RecommendedSQL = created.RollbackSQL
	if d := authorizeFinding(t, exec, ctx, fresh); d.Reason !=
		policy.ReasonAwaitingVerification {
		t.Fatalf("fresh drop of the verified index = %+v, want parked", d)
	}
}

func TestOneChange_RevertOfACreatedIndexIsExempt(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	inFlightMemoriesIndex(t, pool, ctx)
	contract, _ := ContractForActionType("revert_created_index")
	req := policy.ActionRequest{
		SQL:     "DROP INDEX CONCURRENTLY public.idx_memories_live_status_type_quality",
		Feature: string(policy.ChangeIndex), Contract: policyContract(contract),
		TargetObjs: []string{"public.idx_memories_live_status_type_quality"},
	}
	d := exec.StandingPolicyGate().Authorize(ctx, req)
	if d.Reason == policy.ReasonAwaitingVerification || d.VerificationWait != nil {
		t.Fatalf("revert of a created index = %+v, must not wait", d)
	}
}

func TestOneChange_EmergencyFreezeIsExempt(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	first, _ := inFlightMemoriesIndex(t, pool, ctx)
	gate := exec.StandingPolicyGate()
	critical := gate.Authorize(ctx, freezeGateRequest("public.memories", true))
	if critical.Verdict != policy.VerdictExecute || critical.VerificationWait != nil {
		t.Fatalf("critical freeze = %+v, want execute without a wait", critical)
	}
	requireParkedOn(t, gate.Authorize(ctx, freezeGateRequest("public.memories", false)), first)
}

func TestOneChange_OperatorOverrideIsRecorded(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	first, _ := inFlightMemoriesIndex(t, pool, ctx)
	before := waitReleaseCount(exec, "operator_override")
	operator := 42
	sql := memoriesIndexFinding("idx_memories_status_type_quality_current",
		lifeos6411Where).RecommendedSQL
	d, err := exec.authorizeOperatorAction(ctx, sql, 0, &operator)
	if err != nil || d.Verdict != policy.VerdictExecute {
		t.Fatalf("operator approval = %+v, %v; want execute", d, err)
	}
	want := fmt.Sprintf("overrides pending verification of action %d", first)
	if !strings.Contains(d.Detail, want) {
		t.Fatalf("detail %q, want %q", d.Detail, want)
	}
	reason, evidence := decisionEvidence(t, pool, ctx, d.DecisionID)
	if reason != string(policy.ReasonOperatorApproved) ||
		fmt.Sprint(evidenceActionIDs(evidence, "verification_override")) !=
			fmt.Sprint([]int64{first}) {
		t.Fatalf("recorded reason %q evidence %v, want the override of %d", reason,
			evidence, first)
	}
	if got := waitReleaseCount(exec, "operator_override"); got != before+1 {
		t.Fatalf("operator_override releases %d, want %d", got, before+1)
	}
}

func TestOneChange_HardDeadlineReleasesTheWait(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := waitExecutor(t, pool)
	second := waitGUCFinding("work_mem", "10MB")
	// Past the 72 h cap and the hour of grace: released, and recorded.
	stuck := recordInFlight(t, pool, ctx, "ALTER SYSTEM SET work_mem = '9MB'", "",
		"monitoring", 74*time.Hour)
	before := waitReleaseCount(exec, "hard_deadline")
	d := authorizeFinding(t, exec, ctx, second)
	if d.Verdict != policy.VerdictExecute {
		t.Fatalf("past the hard deadline = %+v, want execute", d)
	}
	_, evidence := decisionEvidence(t, pool, ctx, d.DecisionID)
	if fmt.Sprint(evidenceActionIDs(evidence, "verification_wait_released")) !=
		fmt.Sprint([]int64{stuck}) {
		t.Fatalf("evidence %v, want the release of action %d recorded", evidence, stuck)
	}
	if got := waitReleaseCount(exec, "hard_deadline"); got != before+1 {
		t.Fatalf("hard_deadline releases %d, want %d", got, before+1)
	}
	exec.releaseBudget(ctx, d.DecisionID)
	// Inside the cap but past its first window: parked until the deadline.
	mustExec(t, pool, ctx, fmt.Sprintf(`UPDATE sage.action_log SET executed_at =
		now() - interval '70 hours' WHERE id = %d`, stuck))
	parked := authorizeFinding(t, exec, ctx, second)
	requireParkedOn(t, parked, stuck)
	var hard time.Time
	if err := pool.QueryRow(ctx, `SELECT executed_at + interval '73 hours'
		FROM sage.action_log WHERE id = $1`, stuck).Scan(&hard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parked.Detail, hard.UTC().Format(time.RFC3339)) {
		t.Fatalf("detail %q, want until the hard deadline %s", parked.Detail,
			hard.UTC().Format(time.RFC3339))
	}
}

// Proposals racing for one table through the real gate and ledger: one is
// authorized, the others wait for it (its authorization holds the object
// until its action is recorded). The winner's own re-authorization after
// its lease and slot waits is not held up by its own hold.
func TestOneChange_ConcurrentProposalsOnOneTable(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	gate := exec.StandingPolicyGate()
	const racers = 6
	requests := make([]policy.ActionRequest, racers)
	for i := range requests {
		f := memoriesIndexFinding(fmt.Sprintf("idx_memories_race_%d", i), lifeos6411Where)
		requests[i] = findingRequest(f, false)
	}
	decisions := make([]policy.Decision, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range requests {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			decisions[i] = gate.Authorize(ctx, requests[i])
		}(i)
	}
	start.Done()
	done.Wait()
	winner := -1
	for i, d := range decisions {
		if d.Verdict != policy.VerdictExecute {
			continue
		}
		if winner >= 0 {
			t.Fatalf("two proposals on public.memories authorized: %d and %d", winner, i)
		}
		winner = i
	}
	if winner < 0 {
		t.Fatalf("no proposal authorized: %+v", decisions)
	}
	held := fmt.Sprintf("decision %d", decisions[winner].DecisionID)
	for i, d := range decisions {
		if i != winner && (d.Reason != policy.ReasonAwaitingVerification ||
			!strings.Contains(d.Detail, held)) {
			t.Fatalf("loser %d = %+v, want parked on the winner's authorization", i, d)
		}
	}
	if again := gate.Authorize(ctx, requests[winner]); again.Verdict != policy.VerdictExecute {
		t.Fatalf("the winner's re-authorization = %+v, want execute", again)
	}
}

func TestOneChange_ParksAreCountedByReason(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := waitExecutor(t, pool)
	recordInFlight(t, pool, ctx, "ALTER SYSTEM SET work_mem = '9MB'", "", "monitoring",
		time.Minute)
	before := parkCount(exec, string(policy.ReasonAwaitingVerification))
	for i := 0; i < 3; i++ {
		authorizeFinding(t, exec, ctx, waitGUCFinding("work_mem", "10MB"))
	}
	if got := parkCount(exec, string(policy.ReasonAwaitingVerification)); got != before+3 {
		t.Fatalf("awaiting_verification parks %d, want %d", got, before+3)
	}
}

func parkCount(exec *Executor, reason string) int64 {
	for _, c := range ParkCounts() {
		if c.Database == exec.databaseName && c.Reason == reason {
			return c.Count
		}
	}
	return 0
}

func waitReleaseCount(exec *Executor, cause string) int64 {
	for _, c := range WaitReleaseCounts() {
		if c.Database == exec.databaseName && c.Cause == cause {
			return c.Count
		}
	}
	return 0
}

// The in-flight lookup is served by indexes on every table it reads: with
// sequential scans priced out, no sage table is scanned sequentially.
func TestOneChange_InFlightLookupUsesIndexes(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	for i := 0; i < 50; i++ {
		id := recordInFlight(t, pool, ctx, fmt.Sprintf("ANALYZE public.t%d", i), "",
			"success", time.Duration(i)*time.Hour)
		pendingOutcome(t, pool, ctx, id, "analyze")
		concludeVerification(t, pool, ctx, id, "success", "neutral")
	}
	mustExec(t, pool, ctx, "ANALYZE sage.action_log; ANALYZE sage.action_outcome; "+
		"ANALYZE sage.decision")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	var plan string
	rows, err := tx.Query(ctx, "EXPLAIN (FORMAT TEXT) "+inFlightSQL, 720.0, "SELECT 1",
		[]string{}, operatorDecisionIntent)
	if err != nil {
		t.Fatalf("explain the in-flight lookup: %v", err)
	}
	var lines []string
	for rows.Next() {
		if err := rows.Scan(&plan); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, plan)
	}
	rows.Close()
	text := strings.Join(lines, "\n")
	for _, table := range []string{"action_log", "action_outcome", "decision"} {
		if strings.Contains(text, "Seq Scan on "+table) {
			t.Fatalf("the in-flight lookup scans sage.%s sequentially:\n%s", table, text)
		}
	}
}

// Through the executor cycle: the parked candidate is re-evaluated every
// cycle and runs once the verification it waited for concludes.
func TestOneChange_ParkedCandidateResumesAfterTheVerdict(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	fx := newStaleFixtureOn(t, pool, ctx, "autonomous", verifiedDetail(),
		unlimitedWindowPolicy())
	first := recordInFlight(t, pool, ctx,
		"ALTER TABLE public."+fx.table+" SET (autovacuum_vacuum_scale_factor = 0.02)",
		"ALTER TABLE public."+fx.table+" RESET (autovacuum_vacuum_scale_factor)",
		"monitoring", 5*time.Minute)
	pendingOutcome(t, pool, ctx, first, "reloption")

	fx.exec.RunCycle(ctx, false)
	if fx.indexExists(t, fx.index()) || fx.actions(t, fx.f.RecommendedSQL) != 0 {
		t.Fatal("the index was built while its table's change was being verified")
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM sage.decision WHERE verdict = 'parked'
		AND target_objects = $1::jsonb`, `["`+fx.f.ObjectIdentifier+`"]`).
		Scan(&reason); err != nil || reason != string(policy.ReasonAwaitingVerification) {
		t.Fatalf("parked decision reason %q, %v", reason, err)
	}

	concludeVerification(t, pool, ctx, first, "success", "neutral")
	fx.exec.RunCycle(ctx, false)
	if !fx.indexExists(t, fx.index()) || fx.actions(t, fx.f.RecommendedSQL) != 1 {
		t.Fatalf("after the verdict: decisions %v, want the index built",
			fx.decisionVerdicts(t))
	}
}

// An operator's authorization is not a hold: the person's change runs at
// once and holds its object through its action row from then on. (Its
// authorizations are never released, so counting them would hold the
// object for the hold horizon even after a change that ran nothing.)
func TestOneChange_OperatorAuthorizationIsNotAHold(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := waitExecutor(t, pool)
	operator := 7
	d, err := exec.authorizeOperatorAction(ctx, "ALTER SYSTEM SET work_mem = '28MB'", 0,
		&operator)
	if err != nil || d.DecisionID <= 0 {
		t.Fatalf("operator authorization = %+v, %v", d, err)
	}
	next := "ALTER SYSTEM SET work_mem = '32MB'"
	if held, err := exec.VerificationWaits().PendingFor(ctx, next, nil); err != nil ||
		len(held) != 0 {
		t.Fatalf("after an operator authorization: %+v, %v; want no hold", held, err)
	}
	// A self-initiated authorization of the same setting does hold it.
	self := exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(waitGUCFinding("work_mem", "28MB"), false))
	held, err := exec.VerificationWaits().PendingFor(ctx, next, nil)
	if self.Verdict != policy.VerdictExecute || err != nil || len(held) != 1 ||
		held[0].DecisionID != self.DecisionID {
		t.Fatalf("self-initiated %+v: holds %+v, %v; want decision %d holding work_mem",
			self, held, err, self.DecisionID)
	}
}
