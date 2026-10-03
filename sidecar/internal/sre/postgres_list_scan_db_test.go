package sre

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate offender 8: the investigations list (the API's
// /investigations page) read every investigation of the database and
// sorted them to return 50: no index led with the scope and created_at,
// and `$3 IS NULL OR (created_at, id) < ...` kept even a later page from
// being an index range. Every page, with or without a case filter, must
// be an ordered index range of the scope.

func TestList_PagesAreOrderedIndexRanges(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_investigations (deployment_id,
		database_id, id, source_case_id, trigger_kind, trigger_fingerprint, state,
		created_at, updated_at, expires_at)
		SELECT $1::uuid, $2::uuid, gen_random_uuid(), 'case:list:' || (g % 40),
		       'lock_blocking', sha256(('list-' || g)::bytea), 'concluded',
		       now() - g * interval '1 minute', now() - g * interval '1 minute',
		       now() + interval '1 day'
		FROM generate_series(1, 6000) g;
		ANALYZE sage.sre_investigations`, string(scope.DeploymentID),
		string(scope.DatabaseID)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := pool.Config().Copy()
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	tst, err := NewPostgresStore(traced, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	first, err := tst.List(ctx, scope, ListFilter{Limit: 50})
	if err != nil || len(first.Items) != 50 || first.NextCursor == "" {
		t.Fatalf("first page = %d items, cursor %q (%v)", len(first.Items),
			first.NextCursor, err)
	}
	second, err := tst.List(ctx, scope, ListFilter{Limit: 50, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 50 ||
		!second.Items[0].CreatedAt.Before(first.Items[49].CreatedAt) {
		t.Fatalf("second page = %d items (%v), want the next 50 older ones",
			len(second.Items), err)
	}
	byCase, err := tst.List(ctx, scope, ListFilter{Limit: 20, CaseID: "case:list:7"})
	if err != nil || len(byCase.Items) != 20 {
		t.Fatalf("case page = %d items (%v)", len(byCase.Items), err)
	}
	for _, inv := range byCase.Items {
		if inv.CaseID != "case:list:7" {
			t.Fatalf("case page lists %q", inv.CaseID)
		}
	}
	stmts := rec.Matching("FROM sage.sre_investigations", "ORDER BY created_at DESC")
	if len(stmts) != 3 {
		t.Fatalf("list statements = %d, want 3", len(stmts))
	}
	for _, q := range stmts {
		plan, err := testdb.Explain(ctx, pool, "ANALYZE", q.SQL, q.Args...)
		if err != nil {
			t.Fatal(err)
		}
		if plan.SeqScans("sre_investigations") != 0 ||
			plan.Has(func(n testdb.PlanNode) bool { return n.NodeType == "Sort" }) {
			t.Fatalf("a list page scans or sorts the scope's investigations:\n%s", plan)
		}
		var read float64
		plan.Walk(func(n testdb.PlanNode) {
			if n.Relation == "sre_investigations" {
				read += n.ActualRows * n.ActualLoops
			}
		})
		if read > 60 {
			t.Fatalf("a page of at most 51 rows read %v investigations:\n%s", read, plan)
		}
	}
}
