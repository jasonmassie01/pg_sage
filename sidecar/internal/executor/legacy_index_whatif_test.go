package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

// Dogfood lifeos (v1.8.3, action 6383): a covering_index finding written
// by a pre-v1.8.0 optimizer — the LLM's own label was its category — ran
// CREATE INDEX unattended because the what-if gate only looked at
// category missing_index. Every background CREATE INDEX that is not a
// deterministic rule's must carry a verified HypoPG verdict or wait for
// an operator.

// legacyDetail is the detail of a pre-v1.8.0 optimizer finding: LLM
// markers, no what_if_verdict.
func legacyDetail(category string) map[string]any {
	return map[string]any{"queryids": []int64{7}, "category": category,
		"index_type": "btree", "llm_rationale": "covers the hot lookup",
		"plan_source": "query_text_only", "hypopg_validated": false,
		"confidence_score": 0.8, "action_level": "autonomous"}
}

func legacyIndexFinding(category string, detail map[string]any) analyzer.Finding {
	return analyzer.Finding{Category: category, ObjectType: "index",
		ObjectIdentifier: "public.orders",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders " +
			"USING btree (status) INCLUDE (id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_status",
		ActionRisk:  "moderate", Detail: detail}
}

func TestFindingRequest_LegacyOptimizerCategoriesNeedVerifiedWhatIf(t *testing.T) {
	for _, category := range []string{"covering_index", "partial_index",
		"composite_index", "missing_index"} {
		req := findingRequest(legacyIndexFinding(category, legacyDetail(category)), false)
		if !hasApprovalGuardrail(req) {
			t.Errorf("%s without a verdict: no approval guardrail", category)
		}
		verified := legacyDetail(category)
		verified["what_if_verdict"] = "verified"
		verified["hypopg_validated"] = true
		if hasApprovalGuardrail(findingRequest(legacyIndexFinding(category, verified), false)) {
			t.Errorf("%s with a verified verdict still needs approval", category)
		}
	}
}

// The LLM labelled the category freely, so an unknown category — with or
// without LLM markers — fails closed too.
func TestFindingRequest_UnknownIndexCategoryFailsClosed(t *testing.T) {
	cases := map[string]analyzer.Finding{
		"llm label":          legacyIndexFinding("expression_index", legacyDetail("x")),
		"no markers":         legacyIndexFinding("gin_index", map[string]any{}),
		"empty category":     legacyIndexFinding("", nil),
		"migration rewrite":  legacyIndexFinding("migration_safety", map[string]any{}),
		"fk with llm marker": legacyIndexFinding("missing_fk_index", legacyDetail("x")),
	}
	for name, f := range cases {
		if !hasApprovalGuardrail(findingRequest(f, false)) {
			t.Errorf("%s (%q): CREATE INDEX without a verdict had no approval guardrail",
				name, f.Category)
		}
	}
}

// The missing-FK-index rule is deterministic (no LLM, no what-if): its
// request is exactly what it was before the gate widened.
func TestFindingRequest_DeterministicFKIndexUnaffected(t *testing.T) {
	f := analyzer.Finding{Category: "missing_fk_index", ObjectType: "table",
		ObjectIdentifier: "public.orders(customer_id)",
		RecommendedSQL:   `CREATE INDEX CONCURRENTLY ON "public"."orders" ("customer_id");`,
		ActionRisk:       "safe", Detail: map[string]any{"constraint": "orders_cust_fk",
			"fk_column": "customer_id", "referenced_table": "customers"}}
	req := findingRequest(f, false)
	if req.Contract == nil || req.Contract.ActionType != "create_index_concurrently" {
		t.Fatalf("contract = %+v", req.Contract)
	}
	if hasApprovalGuardrail(req) {
		t.Fatal("deterministic FK index gained the what-if approval guardrail")
	}
}

// newLegacyFixture is the stale-approval fixture with a legacy finding.
func newLegacyFixture(t *testing.T, category string, detail map[string]any,
	rollback func(index string) string) *staleFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("legacy_idx_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+
		" (id bigint, status text)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	fx := &staleFixture{pool: pool, ctx: ctx, table: table, database: "legacy_" + table,
		recs: recommendation.NewStore(pool), queue: store.NewActionStore(pool),
		trust: "autonomous"}
	index := fx.index()
	fx.f = analyzer.Finding{Category: category, Severity: "warning", ObjectType: "index",
		ObjectIdentifier: "public." + table, Title: "index recommendation for " + table,
		Recommendation: "covers the hot lookup",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY " + index + " ON public." + table +
			" USING btree (status) INCLUDE (id)",
		RollbackSQL: rollback(index), ActionRisk: "moderate", Detail: detail}
	fx.findingID = fx.insertFinding(t, fx.f)
	t.Cleanup(func() { fx.cleanup() })
	fx.exec = fx.newExecutor(t)
	res, err := fx.recs.Propose(ctx, analyzer.RecommendationProposal(fx.database, fx.f))
	if err != nil || res.Outcome != recommendation.OutcomeCreated {
		t.Fatalf("propose recommendation: %+v, %v", res, err)
	}
	fx.rec = res.Recommendation
	return fx
}

func ownRollback(index string) string {
	return "DROP INDEX CONCURRENTLY IF EXISTS " + index
}

// Action 6383's shape: a legacy covering_index finding with no verdict is
// queued for an operator and never runs.
func TestLegacyCoveringIndexWithoutVerdictIsQueuedNotRun(t *testing.T) {
	fx := newLegacyFixture(t, "covering_index", legacyDetail("covering_index"), ownRollback)
	for range 2 {
		fx.exec.RunCycle(fx.ctx, false)
	}
	q := fx.onlyQueueRow(t)
	if q.Status != "pending" || q.ProposedSQL != fx.f.RecommendedSQL {
		t.Fatalf("queue row = %+v, want one pending proposal of the legacy SQL", q)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 0 || fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d index=%v: an unverified legacy index ran unattended",
			n, fx.indexExists(t, fx.index()))
	}
	got := fx.decisionVerdicts(t)
	if got["queue_approval/approval_required"] == 0 || got["execute/authorized"] != 0 {
		t.Fatalf("decisions = %v, want queue_approval/approval_required only", got)
	}
}

// The same legacy finding with a verified what-if runs autonomously once.
func TestLegacyCoveringIndexWithVerifiedVerdictRuns(t *testing.T) {
	detail := legacyDetail("covering_index")
	detail["what_if_verdict"] = "verified"
	detail["hypopg_validated"] = true
	fx := newLegacyFixture(t, "covering_index", detail, ownRollback)
	fx.exec.RunCycle(fx.ctx, false)
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d index=%v, want one autonomous build",
			n, fx.indexExists(t, fx.index()))
	}
	if rows := fx.queueRows(t); len(rows) != 0 {
		t.Fatalf("queue = %+v, want no approval proposal for a verified index", rows)
	}
	if got := fx.decisionVerdicts(t); got["execute/authorized"] == 0 {
		t.Fatalf("decisions = %v, want an execute verdict", got)
	}
}

// The deterministic FK rule is still authorized by the gate without a
// what-if verdict: no approval proposal, an execute decision, as before.
func TestFKIndexFindingNotRoutedToApproval(t *testing.T) {
	fx := newLegacyFixture(t, "missing_fk_index",
		map[string]any{"queryids": []int64{7}, "constraint": "fk"}, ownRollback)
	fx.exec.RunCycle(fx.ctx, false)
	if rows := fx.queueRows(t); len(rows) != 0 {
		t.Fatalf("queue = %+v: the FK rule was routed to approval", rows)
	}
	got := fx.decisionVerdicts(t)
	if got["execute/authorized"] == 0 || got["queue_approval/approval_required"] != 0 {
		t.Fatalf("decisions = %v, want the gate to authorize the FK index", got)
	}
}
