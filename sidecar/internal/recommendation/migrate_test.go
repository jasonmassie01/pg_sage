package recommendation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// isolatedPool is a freshly bootstrapped database: the migration scans
// every legacy row, so it must not see other tests' fixtures.
func isolatedPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "recmigrate")
	ctx := withTimeout(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func queueRow(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	findingID int64, sql, rollback, status string, decidedBy *int,
) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(finding_id, proposed_sql, rollback_sql, action_risk, status, decided_by,
		 decided_at)
		VALUES ($1, $2, NULLIF($3, ''), 'safe', $4, $5,
		        CASE WHEN $5::int IS NULL THEN NULL ELSE now() END)
		RETURNING id`, findingID, sql, rollback, status, decidedBy).Scan(&id); err != nil {
		t.Fatalf("queue row: %v", err)
	}
	return id
}

// staleInverse is an approval whose inverse SQL no longer matches the
// finding (the C04 shape): the approval covers exactly what was queued.
const staleInverse = "DROP INDEX CONCURRENTLY public.id_old"

type legacyFixture struct {
	plain, pending, approved, stale int64
	pendingQ, approvedQ, staleQ     int
	orphanQ, executedQ              int
}

func seedLegacy(t *testing.T, ctx context.Context, pool *pgxpool.Pool) legacyFixture {
	t.Helper()
	nine, four := 9, 4
	var f legacyFixture
	f.plain = seedFinding(t, ctx, pool, "missing_index", "public.a", sqlA, inverseA)
	f.pending = seedFinding(t, ctx, pool, "missing_index", "public.b",
		"CREATE INDEX CONCURRENTLY ib ON public.b (x)", "DROP INDEX CONCURRENTLY ib")
	f.approved = seedFinding(t, ctx, pool, "missing_index", "public.c",
		"CREATE INDEX CONCURRENTLY ic ON public.c (x)", "DROP INDEX CONCURRENTLY ic")
	f.stale = seedFinding(t, ctx, pool, "missing_index", "public.d",
		"CREATE INDEX CONCURRENTLY id2 ON public.d (y)", "DROP INDEX CONCURRENTLY id2")
	seedFinding(t, ctx, pool, "info_only", "public.e", "", "")
	f.pendingQ = queueRow(t, ctx, pool, f.pending,
		"CREATE INDEX CONCURRENTLY ib ON public.b (x)", "DROP INDEX CONCURRENTLY ib",
		"pending", nil)
	f.approvedQ = queueRow(t, ctx, pool, f.approved,
		"CREATE INDEX CONCURRENTLY ic ON public.c (x)", "DROP INDEX CONCURRENTLY ic",
		"approved", &nine)
	f.staleQ = queueRow(t, ctx, pool, f.stale,
		"CREATE INDEX CONCURRENTLY id2 ON public.d (y)", staleInverse,
		"approved", &four)
	f.orphanQ = queueRow(t, ctx, pool, 99999999, "VACUUM public.z", "", "pending", nil)
	f.executedQ = queueRow(t, ctx, pool, f.plain, sqlA, inverseA, "executed", &nine)
	return f
}

func queueLink(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int) (*int64, *int) {
	t.Helper()
	var recID *int64
	var rev *int
	if err := pool.QueryRow(ctx, `SELECT recommendation_id, recommendation_revision
		FROM sage.action_queue WHERE id=$1`, id).Scan(&recID, &rev); err != nil {
		t.Fatal(err)
	}
	return recID, rev
}

func headForFinding(t *testing.T, ctx context.Context, s *Store, pool *pgxpool.Pool,
	findingID int64) Recommendation {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM sage.recommendation
		WHERE finding_id=$1 ORDER BY id DESC LIMIT 1`, findingID).Scan(&id); err != nil {
		t.Fatalf("no recommendation for finding %d: %v", findingID, err)
	}
	return mustGet(t, ctx, s, id)
}

func TestMigrateLegacyFindingsAndApprovals(t *testing.T) {
	pool, ctx := isolatedPool(t)
	f := seedLegacy(t, ctx, pool)
	s := NewStore(pool)

	report, err := MigrateLegacy(ctx, pool, "prod")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if report.Findings != 4 || report.QueueLinked != 3 || report.ApprovalsMigrated != 2 ||
		report.Skipped != 1 {
		t.Fatalf("report = %+v, want 4 findings, 3 queue links, 2 approvals, 1 skipped", report)
	}
	plain := headForFinding(t, ctx, s, pool, f.plain)
	if plain.State != StateProposed || plain.DatabaseName != "prod" {
		t.Fatalf("plain finding head = %+v", plain)
	}
	revs, _ := s.Revisions(ctx, plain.ID)
	if len(revs) != 1 || revs[0].Source != SourceMigrated || revs[0].InverseSQL != inverseA {
		t.Fatalf("plain revisions = %+v", revs)
	}
	pending := headForFinding(t, ctx, s, pool, f.pending)
	if recID, rev := queueLink(t, ctx, pool, f.pendingQ); recID == nil || *recID != pending.ID ||
		rev == nil || *rev != pending.Revision || pending.State != StateProposed {
		t.Fatalf("pending queue link = %v/%v head=%+v", recID, rev, pending)
	}
	approved := headForFinding(t, ctx, s, pool, f.approved)
	if approved.State != StateApproved || approved.ApprovedBy != "user:9" ||
		approved.ApprovedHash != approved.ContentHash {
		t.Fatalf("approval lost in migration: %+v", approved)
	}
	stale := headForFinding(t, ctx, s, pool, f.stale)
	sr, _ := s.Revisions(ctx, stale.ID)
	cur := sr[len(sr)-1]
	if stale.State != StateApproved || stale.ApprovedBy != "user:4" ||
		cur.InverseSQL != staleInverse || len(sr) != 2 ||
		cur.Source != SourceMigrated || *stale.ApprovedRevision != cur.Revision {
		t.Fatalf("approval of the queued SQL = %+v / %+v", stale, cur)
	}
	if recID, _ := queueLink(t, ctx, pool, f.orphanQ); recID != nil {
		t.Fatal("a queue row with no open finding was linked")
	}
	if recID, _ := queueLink(t, ctx, pool, f.executedQ); recID != nil {
		t.Fatal("an executed queue row was migrated")
	}
	assertMigrationIdempotent(t, ctx, pool)
}

func assertMigrationIdempotent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var heads, revs int
	count := func() {
		_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sage.recommendation),
			(SELECT count(*) FROM sage.recommendation_revision)`).Scan(&heads, &revs)
	}
	count()
	h0, r0 := heads, revs
	again, err := MigrateLegacy(ctx, pool, "prod")
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	count()
	if again.Findings != 0 || again.QueueLinked != 0 || again.ApprovalsMigrated != 0 ||
		heads != h0 || revs != r0 {
		t.Fatalf("second run = %+v, heads %d→%d revs %d→%d; want no changes",
			again, h0, heads, r0, revs)
	}
}

// The analyzer's next unchanged cycle must keep a migrated approval: the
// migration hashes content exactly as the analyzer does.
func TestMigratedApprovalSurvivesUnchangedAnalyzerCycle(t *testing.T) {
	pool, ctx := isolatedPool(t)
	f := seedLegacy(t, ctx, pool)
	s := NewStore(pool)
	if _, err := MigrateLegacy(ctx, pool, "prod"); err != nil {
		t.Fatal(err)
	}
	res := mustPropose(t, ctx, s, Proposal{
		DatabaseName: "prod", Category: "missing_index", Target: "public.c",
		ObjectType: "index", Title: "fresh wording", Severity: "critical",
		ForwardSQL: "CREATE INDEX CONCURRENTLY ic ON public.c (x)",
		InverseSQL: "DROP INDEX CONCURRENTLY ic",
		Evidence:   map[string]any{"seq_scans": 1234},
	})
	head := headForFinding(t, ctx, s, pool, f.approved)
	if res.Outcome != OutcomeUnchanged || head.State != StateApproved {
		t.Fatalf("unchanged cycle after migration: outcome=%s state=%s", res.Outcome, head.State)
	}
	res = mustPropose(t, ctx, s, Proposal{
		DatabaseName: "prod", Category: "missing_index", Target: "public.d",
		ForwardSQL: "CREATE INDEX CONCURRENTLY id2 ON public.d (y)",
		InverseSQL: "DROP INDEX CONCURRENTLY id2",
	})
	stale := headForFinding(t, ctx, s, pool, f.stale)
	if res.Outcome != OutcomeRevised || stale.State != StateProposed || stale.ApprovedHash != "" {
		t.Fatalf("new content after migrated approval: outcome=%s head=%+v, "+
			"want revised and re-approval required", res.Outcome, stale)
	}
}
