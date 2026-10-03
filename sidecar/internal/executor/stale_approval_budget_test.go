package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/schema"
)

// Dogfood lifeos, 2026-10-03 (v1.8.4): finding 18569 (CREATE INDEX on
// public.events) was re-verified and authorized, and its stale approval
// (queue item 3) was pending, yet it never ran. The executor did evaluate
// it every cycle: the gate parked it as blast_radius_exceeded because
// v1.8.3 had touched 21 distinct targets in the rolling 24-hour window
// against max_tables_per_window = 20 (17 duplicate-index drops in leaked
// test_* schemas, a covering index, an ANALYZE, two failed builds and
// work_mem). The limit admitted a 21st table because the window was
// counted before the action, without the action's own table, and an index
// identity ("public.t|btree(c)") counted as a table of its own.

// isolatedSageDB creates a throwaway database with the sage schema, so the
// test owns the whole 24-hour policy window (the shared test database
// carries every other test's actions).
func isolatedSageDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	_, ctx := requireDB(t) // skips without a database; serializes packages
	admin, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	ident := pgx.Identifier{fmt.Sprintf("sage_window_%d", time.Now().UnixNano())}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = ident[0]
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+ident.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config migration: %v", err)
	}
	return pool, ctx
}

// lifeosPolicy is lifeos's standing policy (sage.policy id 1) as stored:
// the legacy single limit of 20 tables, read as the performance budget.
// (It was the unattended profile; the profiles now split that envelope
// between performance and hygiene, so the stored document is the source.)
// spendWindow's decisions carry no budget kind, so they charge the
// performance budget: these tests keep exercising its boundaries.
func lifeosPolicy() policy.Document {
	return lifeosLegacyPolicy()
}

// lifeosWindowTargets are the 21 distinct targets lifeos's executed actions
// held in the window at 19:15 UTC: 17 drops in leaked test schemas, then a
// covering index, an ANALYZE, two failed builds on memories and work_mem.
func lifeosWindowTargets() []string {
	var targets []string
	indexes := []string{"idx_merchant_alias_pattern", "idx_source_event_links_canonical",
		"idx_thesis_allocation_run"}
	for i := 0; len(targets) < 17; i++ {
		schemaName := fmt.Sprintf("test_family_%02d", i/3)
		targets = append(targets, schemaName+"."+indexes[i%3])
	}
	return append(targets, "public.graph_nodes", "public.audit_log", "public.memories",
		"instance")
}

// spendWindow records one executed action per target under the intent, as
// the executor records its own, and returns the action_log ids.
func spendWindow(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, intent string,
	targets ...string,
) []int64 {
	t.Helper()
	decisions := ledger.NewService(ledger.NewPostgresRepository(pool))
	ids := make([]int64, 0, len(targets))
	for _, target := range targets {
		decision, err := decisions.RecordDecision(ctx, ledger.DecisionInput{
			Feature: "index", Intent: intent, Verdict: ledger.VerdictExecute,
			Reason: "authorized", RiskTier: "moderate", PolicyVersion: 1,
			TargetObjects: []string{target},
		})
		if err != nil {
			t.Fatalf("record decision on %s: %v", target, err)
		}
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
			(action_type, sql_executed, outcome, decision_id)
			VALUES ('drop_index', $1, 'success', $2) RETURNING id`,
			"DROP INDEX CONCURRENTLY "+target, decision.ID).Scan(&id); err != nil {
			t.Fatalf("insert action on %s: %v", target, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// ageOut moves an action just past the rolling 24-hour window.
func ageOut(t *testing.T, pool *pgxpool.Pool, ctx context.Context, actionID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET executed_at =
		now() - interval '24 hours 1 minute' WHERE id = $1`, actionID); err != nil {
		t.Fatalf("age out action %d: %v", actionID, err)
	}
}

// assertWaiting checks a cycle left the change waiting for the window: the
// gate recorded why, the stale approval is untouched and nothing ran.
func (fx *staleFixture) assertWaiting(t *testing.T, q queueRow, when string) {
	t.Helper()
	verdicts := fx.decisionVerdicts(t)
	if verdicts["parked/blast_radius_exceeded"] == 0 || verdicts["execute/authorized"] != 0 {
		t.Fatalf("%s: decisions = %v, want parked/blast_radius_exceeded and no execute",
			when, verdicts)
	}
	if after := fx.onlyQueueRow(t); after != q {
		t.Fatalf("%s: queue row = %+v, want the pending proposal unchanged %+v",
			when, after, q)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 0 || fx.indexExists(t, fx.index()) {
		t.Fatalf("%s: actions=%d index=%v, want nothing executed", when, n,
			fx.indexExists(t, fx.index()))
	}
	head, err := fx.recs.Get(fx.ctx, fx.rec.ID)
	if err != nil || head.State != recommendation.StateProposed {
		t.Fatalf("%s: recommendation = %+v (%v), want still proposed", when, head, err)
	}
}

// The lifeos state end to end: a verified, authorized index with a stale
// pending approval waits while the window is full, the reason is recorded,
// and it runs once, superseding the stale approval, when the window has
// room for its table.
func TestBudgetExhaustedVerifiedIndexWaitsThenSupersedesStaleApproval(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	fx := newStaleFixtureOn(t, pool, ctx, "autonomous", unverifiedDetail(), lifeosPolicy())
	q := fx.queuePending(t)
	spent := spendWindow(t, pool, ctx, "index", lifeosWindowTargets()...)
	fx.markVerified(t)

	fx.exec.RunCycle(ctx, false)
	fx.assertWaiting(t, q, "21 of 20 tables")
	// 20 tables remain: the index's table would be the 21st.
	ageOut(t, pool, ctx, spent[0])
	fx.exec.RunCycle(ctx, false)
	fx.assertWaiting(t, q, "20 of 20 tables")
	// 19 tables remain: room for exactly one more.
	ageOut(t, pool, ctx, spent[1])
	fx.exec.RunCycle(ctx, false)

	after := fx.onlyQueueRow(t)
	if after.ID != q.ID || after.Status != "superseded" ||
		!strings.Contains(after.Reason, "approval no longer required: what-if verified") {
		t.Fatalf("queue row = %+v, want %d superseded because what-if verified", after, q.ID)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d index=%v, want one execution that built the index",
			n, fx.indexExists(t, fx.index()))
	}
	usage, err := fx.exec.standingUsage(ctx, policy.ActionRequest{})
	if err != nil || usage.TablesInWindow != 20 || usage.SelfInitiatedChangesInWindow != 20 {
		t.Fatalf("usage = %+v (%v), want 20 tables and 20 changes: never past the limit",
			usage, err)
	}
	fx.exec.RunCycle(ctx, false)
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 {
		t.Fatalf("after another cycle: actions=%d, want 1", n)
	}
}

// Re-touching a table the window already counts does not widen the blast
// radius, also when an earlier change recorded it under an index identity.
func TestBlastRadiusRetouchingAWindowTableDoesNotWiden(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	doc := lifeosPolicy()
	doc.BlastRadius.MaxTablesPerWindow = 2
	fx := newStaleFixtureOn(t, pool, ctx, "autonomous", verifiedDetail(), doc)
	spendWindow(t, pool, ctx, "index",
		"public."+fx.table+"|btree(id)", "public."+fx.table, "other_schema.other_table")

	fx.exec.RunCycle(ctx, false)

	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d decisions=%v, want the index built on a counted table",
			n, fx.decisionVerdicts(t))
	}
	if got := fx.decisionVerdicts(t)["parked/blast_radius_exceeded"]; got != 0 {
		t.Fatalf("decisions = %v, want no blast-radius park", fx.decisionVerdicts(t))
	}
}

// Boundaries of max_tables_per_window: a new table at exactly the limit
// parks, and a zero limit admits no table at all, also in an empty window.
func TestBlastRadiusNewTableBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		limit   int64
		targets []string
	}{
		{"new table at the limit", 2, []string{"a_schema.a_table", "b_schema.b_table"}},
		{"zero limit, empty window", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := isolatedSageDB(t)
			doc := lifeosPolicy()
			doc.BlastRadius.MaxTablesPerWindow = tc.limit
			fx := newStaleFixtureOn(t, pool, ctx, "autonomous", verifiedDetail(), doc)
			spendWindow(t, pool, ctx, "index", tc.targets...)

			fx.exec.RunCycle(ctx, false)

			verdicts := fx.decisionVerdicts(t)
			if verdicts["parked/blast_radius_exceeded"] != 1 || fx.indexExists(t, fx.index()) {
				t.Fatalf("decisions = %v index=%v, want one park and no index",
					verdicts, fx.indexExists(t, fx.index()))
			}
		})
	}
}
