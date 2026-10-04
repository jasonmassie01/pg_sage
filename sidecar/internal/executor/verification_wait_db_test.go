package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// One self-initiated change per object, against real Postgres. Both lifeos
// sequences of 2026-10-04 are replayed: action 6409 (work_mem 10MB) while
// 6407 (work_mem 9MB) was being judged, and index 6411 on public.memories
// ten minutes after 6410, which it subsumes, while 6410 was being verified.

// waitExecutor is an autonomous executor whose real standing gate reads an
// open policy and the isolated database's in-flight verifications.
func waitExecutor(t *testing.T, pool *pgxpool.Pool) *Executor {
	t.Helper()
	exec := budgetExecutor(t, pool, unlimitedWindowPolicy())
	exec.WithDatabaseName(fmt.Sprintf("wait_%d", time.Now().UnixNano()))
	return exec
}

func lifeosTables(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	mustExec(t, pool, ctx, `CREATE TABLE public.memories (id bigint, status text,
		fact_type text, quality_score real, valid_to timestamptz, deleted_at timestamptz);
		CREATE TABLE public.events (id bigint, kind text);
		CREATE INDEX idx_memories_live_status_type_quality ON public.memories
		  (status, fact_type, quality_score)
		  WHERE valid_to IS NULL AND deleted_at IS NULL AND quality_score IS NOT NULL;`)
}

// recordInFlight writes an executed action as the executor does, aged by
// age, with its lifecycle outcome.
func recordInFlight(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	sql, rollback, outcome string, age time.Duration) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, executed_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, now() - make_interval(secs => $5))
		RETURNING id`, categorizeAction(sql), sql, rollback, outcome,
		age.Seconds()).Scan(&id); err != nil {
		t.Fatalf("record in-flight action %q: %v", sql, err)
	}
	return id
}

// pendingOutcome records the action's prediction, its verdict pending.
func pendingOutcome(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id int64,
	class string) {
	t.Helper()
	mustExec(t, pool, ctx, fmt.Sprintf(`INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method)
		VALUES (%d, '%s', '{}'::jsonb, 'rule')`, id, class))
}

// concludeVerification records the verdict and the lifecycle outcome.
func concludeVerification(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id int64,
	outcome, verdict string) {
	t.Helper()
	mustExec(t, pool, ctx, fmt.Sprintf(`UPDATE sage.action_log SET outcome = '%s'
		WHERE id = %d; UPDATE sage.action_outcome SET verdict = '%s', decided_at = now()
		WHERE action_log_id = %d`, outcome, id, verdict, id))
}

func waitGUCFinding(name, value string) analyzer.Finding {
	return analyzer.Finding{Category: "config_tuning", ObjectType: "configuration",
		ObjectIdentifier: "instance", Title: "raise " + name,
		RecommendedSQL: fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", name, value)}
}

func memoriesIndexFinding(name, where string) analyzer.Finding {
	return analyzer.Finding{Category: "missing_index", ObjectType: "index",
		ObjectIdentifier: "public.memories|btree(status,fact_type,quality_score)",
		Title:            "index " + name,
		RecommendedSQL: "CREATE INDEX CONCURRENTLY " + name +
			" ON public.memories (status, fact_type, quality_score) WHERE " + where,
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS public." + name,
		Detail:      verifiedDetail()}
}

const (
	lifeos6410Where = "valid_to IS NULL AND deleted_at IS NULL AND quality_score IS NOT NULL"
	lifeos6411Where = "valid_to IS NULL AND deleted_at IS NULL"
)

func authorizeFinding(t *testing.T, exec *Executor, ctx context.Context,
	f analyzer.Finding) policy.Decision {
	t.Helper()
	return exec.StandingPolicyGate().Authorize(ctx, findingRequest(f, false))
}

// requireExecutable proves the control: without the in-flight change the
// request would run (Explain records nothing and holds no slot).
func requireExecutable(t *testing.T, exec *Executor, ctx context.Context,
	f analyzer.Finding) {
	t.Helper()
	d := exec.StandingPolicyGate().(policy.Explainer).Explain(ctx, findingRequest(f, false))
	if d.Verdict != policy.VerdictExecute {
		t.Fatalf("control: %q = %+v, want execute", f.RecommendedSQL, d)
	}
}

func requireParkedOn(t *testing.T, d policy.Decision, actionID int64) {
	t.Helper()
	want := fmt.Sprintf("awaiting verification of action %d (until ", actionID)
	if d.Verdict != policy.VerdictPark || d.Reason != policy.ReasonAwaitingVerification ||
		!strings.Contains(d.Detail, want) {
		t.Fatalf("decision %+v, want parked awaiting verification of action %d", d, actionID)
	}
}

func decisionEvidence(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	id int64) (string, map[string]any) {
	t.Helper()
	var reason string
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT reason, evidence FROM sage.decision
		WHERE id = $1`, id).Scan(&reason, &raw); err != nil {
		t.Fatalf("read decision %d: %v", id, err)
	}
	evidence := map[string]any{}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatalf("decode decision %d evidence: %v", id, err)
	}
	return reason, evidence
}

// evidenceActionIDs lists the action ids under evidence[key].
func evidenceActionIDs(evidence map[string]any, key string) []int64 {
	items, _ := evidence[key].([]any)
	var ids []int64
	for _, item := range items {
		entry, _ := item.(map[string]any)
		switch id := entry["action_id"].(type) {
		case float64: // decoded from the ledger's jsonb
			ids = append(ids, int64(id))
		case int64: // as the ledger input holds it
			ids = append(ids, id)
		}
	}
	return ids
}

func TestOneChange_LifeosWorkMemSequence(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := waitExecutor(t, pool)
	second := waitGUCFinding("work_mem", "10MB")
	requireExecutable(t, exec, ctx, second)

	// 17:55 action 6407; 18:34 the new finding for 10MB arrives.
	first := recordInFlight(t, pool, ctx, "ALTER SYSTEM SET work_mem = '9MB'",
		"ALTER SYSTEM RESET work_mem", "monitoring", 39*time.Minute)
	pendingOutcome(t, pool, ctx, first, "guc")

	parked := authorizeFinding(t, exec, ctx, second)
	requireParkedOn(t, parked, first)
	reason, evidence := decisionEvidence(t, pool, ctx, parked.DecisionID)
	if reason != string(policy.ReasonAwaitingVerification) ||
		fmt.Sprint(evidenceActionIDs(evidence, "verification_wait")) != fmt.Sprint([]int64{first}) {
		t.Fatalf("recorded reason %q evidence %v, want the wait on action %d", reason,
			evidence, first)
	}
	if detail, _ := evidence["verification_wait_detail"].(string); !strings.Contains(detail,
		"guc:work_mem") {
		t.Fatalf("recorded detail %q, want the object named", detail)
	}
	// Another setting is another object.
	other := waitGUCFinding("random_page_cost", "1.1")
	requireExecutable(t, exec, ctx, other)
	if d := authorizeFinding(t, exec, ctx, other); d.Verdict != policy.VerdictExecute {
		t.Fatalf("random_page_cost while work_mem is verified = %+v, want execute", d)
	}

	concludeVerification(t, pool, ctx, first, "success", "improved")
	if d := authorizeFinding(t, exec, ctx, second); d.Verdict != policy.VerdictExecute {
		t.Fatalf("after the verdict = %+v, want the change to resume", d)
	}
}

func TestOneChange_LifeosMemoriesIndexSequence(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	second := memoriesIndexFinding("idx_memories_status_type_quality_current",
		lifeos6411Where)
	requireExecutable(t, exec, ctx, second)

	created := memoriesIndexFinding("idx_memories_live_status_type_quality", lifeos6410Where)
	first := recordInFlight(t, pool, ctx, created.RecommendedSQL, created.RollbackSQL,
		"monitoring", 10*time.Minute)
	pendingOutcome(t, pool, ctx, first, "index_create")

	requireParkedOn(t, authorizeFinding(t, exec, ctx, second), first)

	// Another table is another object.
	events := analyzer.Finding{Category: "missing_index", ObjectType: "index",
		ObjectIdentifier: "public.events|btree(kind)", Title: "index events",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY idx_events_kind ON public.events (kind)",
		RollbackSQL:    "DROP INDEX CONCURRENTLY IF EXISTS public.idx_events_kind",
		Detail:         verifiedDetail()}
	requireExecutable(t, exec, ctx, events)
	if d := authorizeFinding(t, exec, ctx, events); d.Verdict != policy.VerdictExecute {
		t.Fatalf("index on public.events = %+v, want execute", d)
	}

	// A regression rolled 6410 back: its verification concluded.
	concludeVerification(t, pool, ctx, first, "rolled_back", "regressed")
	if d := authorizeFinding(t, exec, ctx, second); d.Verdict != policy.VerdictExecute {
		t.Fatalf("after the rollback = %+v, want the change to resume", d)
	}
}

// The outcome ledger alone holds the object: action_log may already read
// success while the verdict is still pending. A rolled-back action whose
// verdict never came holds nothing.
func TestOneChange_PendingOutcomeAloneHolds(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := waitExecutor(t, pool)
	second := waitGUCFinding("work_mem", "10MB")
	first := recordInFlight(t, pool, ctx, "ALTER SYSTEM SET work_mem = '9MB'", "",
		"success", 20*time.Minute)
	pendingOutcome(t, pool, ctx, first, "guc")
	requireParkedOn(t, authorizeFinding(t, exec, ctx, second), first)

	mustExec(t, pool, ctx, fmt.Sprintf(`UPDATE sage.action_log SET outcome = 'rolled_back'
		WHERE id = %d`, first))
	if d := authorizeFinding(t, exec, ctx, second); d.Verdict != policy.VerdictExecute {
		t.Fatalf("rolled back with a pending verdict = %+v, want execute", d)
	}
}

func TestOneChange_InFlightLifecycleStates(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := waitExecutor(t, pool)
	second := waitGUCFinding("work_mem", "10MB")
	for _, outcome := range []string{"monitoring", "pending", "interrupted", "rolling_back"} {
		id := recordInFlight(t, pool, ctx, "ALTER SYSTEM SET work_mem = '9MB'", "",
			outcome, time.Minute)
		requireParkedOn(t, authorizeFinding(t, exec, ctx, second), id)
		mustExec(t, pool, ctx, fmt.Sprintf(`UPDATE sage.action_log SET outcome = 'success'
			WHERE id = %d`, id))
	}
	for _, outcome := range []string{"success", "unverifiable", "failed", "rolled_back",
		"reverted"} {
		recordInFlight(t, pool, ctx, "ALTER SYSTEM SET work_mem = '9MB'", "", outcome,
			time.Minute)
	}
	if d := authorizeFinding(t, exec, ctx, second); d.Verdict != policy.VerdictExecute {
		t.Fatalf("only concluded actions = %+v, want execute", d)
	}
}

// A dropped index is no longer in the catalog: its table comes from the
// kept definition (the drop's rollback re-creates it ON the table).
func TestOneChange_DroppedIndexIdentifiesItsTable(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	second := memoriesIndexFinding("idx_memories_status_type_quality_current",
		lifeos6411Where)
	first := recordInFlight(t, pool, ctx, "DROP INDEX CONCURRENTLY public.idx_memories_gone",
		"CREATE INDEX CONCURRENTLY idx_memories_gone ON public.memories USING btree (status)",
		"monitoring", time.Hour)
	requireParkedOn(t, authorizeFinding(t, exec, ctx, second), first)
}

// A change naming an index is a change to its table: a drop or reindex of
// another index on public.memories waits for 6410's verification.
func TestOneChange_IndexNameResolvesToItsTable(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	mustExec(t, pool, ctx, `CREATE INDEX idx_memories_type ON public.memories (fact_type)`)
	exec := waitExecutor(t, pool)
	created := memoriesIndexFinding("idx_memories_new", lifeos6410Where)
	first := recordInFlight(t, pool, ctx, created.RecommendedSQL, created.RollbackSQL,
		"monitoring", time.Minute)
	drop := dropIndexFinding("public.idx_memories_type")
	requireParkedOn(t, authorizeFinding(t, exec, ctx, drop), first)
	// And the reverse: an in-flight change named by index, a request by table.
	concludeVerification(t, pool, ctx, first, "success", "improved")
	byIndex := recordInFlight(t, pool, ctx,
		"REINDEX INDEX CONCURRENTLY public.idx_memories_live_status_type_quality", "",
		"monitoring", time.Minute)
	analyze := analyzer.Finding{Category: "stale_statistics", ObjectType: "table",
		ObjectIdentifier: "public.memories", Title: "analyze memories",
		RecommendedSQL: "ANALYZE public.memories"}
	requireParkedOn(t, authorizeFinding(t, exec, ctx, analyze), byIndex)
}

// Quoted and unqualified spellings name the same table.
func TestOneChange_SpellingsOfOneTableMatch(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	first := recordInFlight(t, pool, ctx,
		`ALTER TABLE "public"."memories" SET (autovacuum_vacuum_scale_factor = 0.02)`,
		`ALTER TABLE "public"."memories" RESET (autovacuum_vacuum_scale_factor)`,
		"monitoring", time.Minute)
	second := memoriesIndexFinding("idx_memories_quoted", lifeos6411Where)
	second.RecommendedSQL = strings.Replace(second.RecommendedSQL, "public.memories",
		"memories", 1)
	requireParkedOn(t, authorizeFinding(t, exec, ctx, second), first)
}

// The approval card reads the same lookup by SQL and targets.
func TestOneChange_PendingForServesTheCard(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	created := memoriesIndexFinding("idx_memories_live_status_type_quality", lifeos6410Where)
	first := recordInFlight(t, pool, ctx, created.RecommendedSQL, created.RollbackSQL,
		"monitoring", time.Minute)
	second := memoriesIndexFinding("idx_memories_status_type_quality_current",
		lifeos6411Where)
	got, err := exec.VerificationWaits().PendingFor(ctx, second.RecommendedSQL,
		[]string{second.ObjectIdentifier})
	if err != nil || len(got) != 1 || got[0].ActionID != first ||
		got[0].Object != "table:public.memories" || got[0].Until.IsZero() ||
		got[0].HardDeadline.Before(got[0].Until) {
		t.Fatalf("PendingFor = %+v, %v; want action %d on table:public.memories", got, err,
			first)
	}
	none, err := exec.VerificationWaits().PendingFor(ctx,
		"CREATE INDEX CONCURRENTLY i ON public.events (kind)", nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("PendingFor(events) = %+v, %v; want none", none, err)
	}
}
