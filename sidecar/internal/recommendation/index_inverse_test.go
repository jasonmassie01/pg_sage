package recommendation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood lifeos (finding 18007): releases before v1.8.0 stored the LLM's
// drop_ddl as rollback_sql and refreshed recommended_sql without it (C04),
// so an open finding paired CREATE INDEX idx_memories_active_query_opt with
// a rollback dropping idx_memories_active_partial. MigrateLegacy copied
// that pair into the recommendation revision the executor acts on.

const (
	createOpt = "CREATE INDEX CONCURRENTLY idx_memories_active_query_opt ON public.memories " +
		"(status, fact_type, quality_score) WHERE (valid_to IS NULL AND deleted_at IS NULL)"
	stalePartialDrop = "DROP INDEX CONCURRENTLY IF EXISTS idx_memories_active_partial"
	derivedOptDrop   = `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_memories_active_query_opt"`
)

func TestInverseDropsCreatedIndex(t *testing.T) {
	create := "CREATE INDEX CONCURRENTLY idx_q ON public.memories (status)"
	cases := []struct {
		name, forward, inverse string
		want                   bool
	}{
		{"same name", create, "DROP INDEX CONCURRENTLY IF EXISTS idx_q", true},
		{"qualified", create, `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_q"`, true},
		{"not concurrent", create, "DROP INDEX public.idx_q;", true},
		{"restrict", create, "DROP INDEX CONCURRENTLY idx_q RESTRICT", true},
		{"case folded", create, "drop index concurrently IDX_Q", true},
		{"quoted forward", `CREATE INDEX CONCURRENTLY "Idx_Q" ON public.m (s)`,
			`DROP INDEX CONCURRENTLY "Idx_Q"`, true},
		{"quoted case differs", `CREATE INDEX CONCURRENTLY "Idx_Q" ON public.m (s)`,
			"DROP INDEX CONCURRENTLY idx_q", false},
		{"other index", create, "DROP INDEX CONCURRENTLY idx_memories_active_partial", false},
		{"other schema", create, "DROP INDEX CONCURRENTLY sage.idx_q", false},
		{"cascade", create, "DROP INDEX CONCURRENTLY idx_q CASCADE", false},
		{"two statements", create, "DROP INDEX idx_q; DROP TABLE public.memories", false},
		{"two indexes", create, "DROP INDEX idx_q, idx_other", false},
		{"not a drop", create, "SELECT 1", false},
		{"empty inverse", create, "", false},
		{"unqualified create, qualified drop",
			"CREATE INDEX CONCURRENTLY idx_q ON memories (s)", "DROP INDEX public.idx_q", true},
	}
	for _, c := range cases {
		if got := InverseDropsCreatedIndex(c.forward, c.inverse); got != c.want {
			t.Errorf("%s: InverseDropsCreatedIndex(%q, %q) = %t, want %t",
				c.name, c.forward, c.inverse, got, c.want)
		}
	}
}

func TestDerivedIndexInverse(t *testing.T) {
	cases := []struct {
		forward, want string
		ok            bool
	}{
		{createOpt, derivedOptDrop, true},
		{"CREATE INDEX CONCURRENTLY idx_q ON memories (s)",
			`DROP INDEX CONCURRENTLY IF EXISTS "idx_q"`, true},
		{`CREATE INDEX CONCURRENTLY "We""ird" ON "S"."t" (s)`,
			`DROP INDEX CONCURRENTLY IF EXISTS "S"."We""ird"`, true},
		// No name: the server picks it, so no rollback can be derived.
		{`CREATE INDEX CONCURRENTLY ON "public"."orders" ("customer_id");`, "", false},
		{"ANALYZE public.orders", "", false},
		{"", "", false},
		{"CREATE INDEX CONCURRENTLY broken ON", "", false},
	}
	for _, c := range cases {
		got, ok := DerivedIndexInverse(c.forward)
		if got != c.want || ok != c.ok {
			t.Errorf("DerivedIndexInverse(%q) = %q,%t want %q,%t", c.forward, got, ok,
				c.want, c.ok)
		}
	}
}

func findingRollback(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var rollback *string
	if err := pool.QueryRow(ctx, `SELECT rollback_sql FROM sage.findings WHERE id=$1`,
		id).Scan(&rollback); err != nil {
		t.Fatal(err)
	}
	if rollback == nil {
		return ""
	}
	return *rollback
}

func currentRevision(t *testing.T, ctx context.Context, s *Store, id int64) Revision {
	t.Helper()
	revs, err := s.Revisions(ctx, id)
	if err != nil || len(revs) == 0 {
		t.Fatalf("revisions of %d: %v", id, err)
	}
	return revs[len(revs)-1]
}

// A not-yet-migrated finding is repaired before it is migrated, so its
// first revision already pairs the create with its own drop.
func TestMigrateLegacyRepairsStaleFindingRollback(t *testing.T) {
	pool, ctx := isolatedPool(t)
	s := NewStore(pool)
	stale := seedFinding(t, ctx, pool, "partial_index", "public.memories",
		createOpt, stalePartialDrop)
	good := seedFinding(t, ctx, pool, "missing_index", "public.a", sqlA, inverseA)
	none := seedFinding(t, ctx, pool, "missing_fk_index", "public.o(c)",
		`CREATE INDEX CONCURRENTLY ON "public"."o" ("c")`, "")

	report, err := MigrateLegacy(ctx, pool, "lifeos")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := findingRollback(t, ctx, pool, stale); got != derivedOptDrop {
		t.Fatalf("stale finding rollback = %q, want %q", got, derivedOptDrop)
	}
	if got := findingRollback(t, ctx, pool, good); got != inverseA {
		t.Fatalf("consistent rollback rewritten: %q", got)
	}
	if got := findingRollback(t, ctx, pool, none); got != "" {
		t.Fatalf("unnamed index gained a rollback: %q", got)
	}
	rev := currentRevision(t, ctx, s, headForFinding(t, ctx, s, pool, stale).ID)
	if rev.ForwardSQL != createOpt || rev.InverseSQL != derivedOptDrop {
		t.Fatalf("migrated revision = %q / %q", rev.ForwardSQL, rev.InverseSQL)
	}
	if report.InversesRepaired != 1 || report.Findings != 3 {
		t.Fatalf("report = %+v, want 1 inverse repaired, 3 findings migrated", report)
	}
	assertMigrationIdempotent(t, ctx, pool)
}

// Finding 18007's actual state: already migrated (on 2026-10-02) into a
// proposed recommendation whose revision holds the stale inverse. The
// repair appends a corrected revision through the normal revise path.
func TestMigrateLegacyRevisesAlreadyMigratedStaleInverse(t *testing.T) {
	pool, ctx := isolatedPool(t)
	s := NewStore(pool)
	findingID := seedFinding(t, ctx, pool, "partial_index", "public.memories",
		createOpt, stalePartialDrop)
	res := mustPropose(t, ctx, s, Proposal{DatabaseName: "lifeos",
		Category: "partial_index", Target: "public.memories", ObjectType: "index",
		ForwardSQL: createOpt, InverseSQL: stalePartialDrop})
	if res.Outcome != OutcomeCreated {
		t.Fatalf("seed recommendation: %+v", res)
	}

	report, err := MigrateLegacy(ctx, pool, "lifeos")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	head := mustGet(t, ctx, s, res.Recommendation.ID)
	rev := currentRevision(t, ctx, s, head.ID)
	if head.Revision != 2 || head.State != StateProposed || rev.InverseSQL != derivedOptDrop ||
		rev.ForwardSQL != createOpt || rev.Source != SourceMigrated {
		t.Fatalf("head = %+v revision = %+v, want revision 2 with the derived inverse",
			head, rev)
	}
	if got := findingRollback(t, ctx, pool, findingID); got != derivedOptDrop {
		t.Fatalf("finding rollback = %q", got)
	}
	if report.InversesRepaired != 2 {
		t.Fatalf("report = %+v, want the finding and the revision repaired", report)
	}
	assertMigrationIdempotent(t, ctx, pool)
}

// An operator's approval pins exactly the content approved (C04): an
// approved stale pair is not rewritten under the approval. It stays as
// approved, and the executor refuses to run it (ErrRollbackMismatch).
func TestMigrateLegacyLeavesApprovedContentAlone(t *testing.T) {
	pool, ctx := isolatedPool(t)
	s := NewStore(pool)
	seedFinding(t, ctx, pool, "partial_index", "public.memories", createOpt, derivedOptDrop)
	res := mustPropose(t, ctx, s, Proposal{DatabaseName: "lifeos",
		Category: "partial_index", Target: "public.memories", ObjectType: "index",
		ForwardSQL: createOpt, InverseSQL: stalePartialDrop})
	setState(t, ctx, pool, res.Recommendation.ID, StateApproved)

	if _, err := MigrateLegacy(ctx, pool, "lifeos"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	head := mustGet(t, ctx, s, res.Recommendation.ID)
	if head.Revision != 1 || head.State != StateApproved ||
		currentRevision(t, ctx, s, head.ID).InverseSQL != stalePartialDrop {
		t.Fatalf("approved content changed under its approval: %+v", head)
	}
}
