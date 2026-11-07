package facts

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// facts-03: the app-managed detector names an index as the catalog does
// (EF Core's "IX_Orders_CustomerId"), so the fact an operator confirms
// binds pg_sage's drop of that index.

// No concurrent-access tests: the detector is a stateless read of the
// drop history; the store's concurrency is covered in store_db_test.go.

// quotedIndexTable creates schema.table (both quoted as given) and
// returns the qualified table.
func quotedIndexTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	schema, table string) string {
	t.Helper()
	qs, qt := sanitize.QuoteIdentifier(schema), sanitize.QuoteQualifiedName(schema, table)
	if schema != "public" {
		execAll(t, ctx, pool, "CREATE SCHEMA "+qs)
	}
	execAll(t, ctx, pool, "CREATE TABLE "+qt+" (a int, b int, c int)")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DROP TABLE IF EXISTS "+qt)
		if schema != "public" {
			_, _ = pool.Exec(bg, "DROP SCHEMA IF EXISTS "+qs+" CASCADE")
		}
	})
	return qt
}

// recordQuotedDrop creates index on table, then writes drops executed
// pg_sage drops of it, quoted as pg_sage writes them.
func recordQuotedDrop(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	schema, index, table, cols string, drops int) {
	t.Helper()
	qi := sanitize.QuoteQualifiedName(schema, index)
	execAll(t, ctx, pool, fmt.Sprintf("CREATE INDEX %s ON %s (%s)",
		sanitize.QuoteIdentifier(index), table, cols))
	for i := 0; i < drops; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type,
			sql_executed, rollback_sql, outcome, executed_at)
			SELECT 'drop_index', $1, pg_get_indexdef(to_regclass($2)), 'success',
			       now() - make_interval(mins => $3)`,
			"DROP INDEX CONCURRENTLY "+qi+";", qi, 10*(i+1)); err != nil {
			t.Fatalf("record drop of %s: %v", qi, err)
		}
	}
}

// tableRef is a table as fact subjects and evidence spell it.
func tableRef(schema, table string) string {
	return ObjectRef{Kind: KindTable, Schema: schema, Name: table}.String()
}

// confirmAndBind proposes p, confirms it and binds pg_sage's drop sql.
func confirmAndBind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, p Proposal,
	sql string) (Fact, []policy.FactBinding) {
	t.Helper()
	s := NewStore(pool)
	f, _, err := s.Propose(ctx, p)
	if err != nil {
		t.Fatalf("propose %q: %v", p.Subject, err)
	}
	if f.Subject != p.Subject {
		t.Fatalf("stored subject %q, detector proposed %q", f.Subject, p.Subject)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true,
		Actor: "alice@example.com"}); err != nil {
		t.Fatalf("confirm %q: %v", p.Subject, err)
	}
	b := NewBinder(s, NewCatalogResolver(pool), nil)
	out, err := b.Bind(ctx, policy.ActionRequest{SQL: sql,
		Contract: &policy.ActionContract{ActionType: "drop_unused_index",
			RiskTier: policy.RiskSafe}})
	if err != nil {
		t.Fatalf("bind %q: %v", sql, err)
	}
	return f, out
}

// The EF Core case from the plan: "public"."IX_Orders_CustomerId" is
// proposed as public."IX_Orders_CustomerId", and the confirmed fact binds
// DROP INDEX CONCURRENTLY public."IX_Orders_CustomerId". It is still there,
// so re-verification keeps it (the lower-cased subject never matched).
func TestAppManagedDetectorKeepsCatalogCaseAndBinds(t *testing.T) {
	pool, ctx := livePool(t)
	table := uniqueName("Orders_")
	qt := quotedIndexTable(t, ctx, pool, "public", table)
	idx := "IX_Orders_CustomerId_" + table
	recordQuotedDrop(t, ctx, pool, "public", idx, qt, "a", 3)
	got, err := NewAppManagedDetector(pool).Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subject := `public."` + idx + `"`
	p, ok := proposalFor(got, TypeAppMigrations, subject)
	if !ok || len(got) != 1 || p.Kind != KindIndex || len(p.Evidence) != 1 ||
		p.Evidence[0].Ref != "drop_index:"+subject ||
		!strings.Contains(p.Evidence[0].Detail, "3 times") ||
		!strings.Contains(p.Evidence[0].Detail, "on table "+tableRef("public", table)) {
		t.Fatalf("proposals %+v, want one for %s citing 3 drops on table %s", got,
			subject, table)
	}
	f, out := confirmAndBind(t, ctx, pool, p, "DROP INDEX CONCURRENTLY "+subject)
	if len(out) != 1 || out[0].FactID != f.ID || out[0].Route != string(RouteSourceFix) {
		t.Fatalf("drop of %s bindings %+v, want fact %d routed to source_fix", subject,
			out, f.ID)
	}
	expired, err := NewStore(pool).Reverify(ctx, time.Now())
	if err != nil || len(expired) != 0 {
		t.Fatalf("re-verify of a present index expired %+v (%v)", expired, err)
	}
}

// Invalid input: names with a space, a dot or a double quote, in a
// mixed-case schema, round-trip through the subject and the binding.
func TestAppManagedDetectorOddNamesRoundTrip(t *testing.T) {
	pool, ctx := livePool(t)
	schema := uniqueName("EfCore ")
	qt := quotedIndexTable(t, ctx, pool, schema, "Orders")
	names := []string{"my idx", "a.b", `q"x`}
	for i, idx := range names {
		recordQuotedDrop(t, ctx, pool, schema, idx, qt, []string{"a", "b", "c"}[i], 1)
	}
	got, err := NewAppManagedDetector(pool).Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(names) {
		t.Fatalf("proposals %+v, want one per index %v", got, names)
	}
	for _, idx := range names {
		subject := ObjectRef{Kind: KindIndex, Schema: schema, Name: idx}.String()
		p, ok := proposalFor(got, TypeAppMigrations, subject)
		if !ok {
			t.Fatalf("no proposal for %s in %+v", subject, got)
		}
		norm, err := Validate(p)
		if err != nil || norm.Subject != subject {
			t.Fatalf("subject %s validates to %q (%v)", subject, norm.Subject, err)
		}
		drop := "DROP INDEX CONCURRENTLY " + sanitize.QuoteQualifiedName(schema, idx)
		f, out := confirmAndBind(t, ctx, pool, p, drop)
		if len(out) != 1 || out[0].FactID != f.ID {
			t.Fatalf("%s bindings %+v, want fact %d", drop, out, f.ID)
		}
	}
}

// Nil/empty: an index pg_sage dropped that is still gone (no current
// definition), a drop that named no schema and an index whose name holds
// '*' give no proposal.
func TestAppManagedDetectorSkipsGoneAndUnqualifiedDrops(t *testing.T) {
	pool, ctx := livePool(t)
	table := uniqueName("am_skip_")
	qt := quotedIndexTable(t, ctx, pool, "public", table)
	gone := "IX_Gone_" + table
	recordQuotedDrop(t, ctx, pool, "public", gone, qt, "a", 2)
	execAll(t, ctx, pool, `DROP INDEX public."`+gone+`"`)
	bare := "idx_bare_" + table
	execAll(t, ctx, pool, fmt.Sprintf("CREATE INDEX %s ON %s (b)", bare, qt))
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		rollback_sql, outcome) SELECT 'drop_index', $1, pg_get_indexdef(to_regclass($2)),
		'success'`, "DROP INDEX CONCURRENTLY "+bare, "public."+bare); err != nil {
		t.Fatal(err)
	}
	// '*' is a subject wildcard: the subject would cover other indexes.
	recordQuotedDrop(t, ctx, pool, "public", "IX*_"+table, qt, "c", 1)
	got, err := NewAppManagedDetector(pool).Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("proposals %+v, want none for a gone index, an unqualified drop or a "+
			"name with '*'", got)
	}
}

// The model's evidence cites the index as the detector names it, with its
// table.
func TestCollectModelEvidenceCitesCatalogCasedIndex(t *testing.T) {
	pool, ctx := livePool(t)
	table := uniqueName("Orders_")
	qt := quotedIndexTable(t, ctx, pool, "public", table)
	idx := "IX_Orders_B_" + table
	recordQuotedDrop(t, ctx, pool, "public", idx, qt, "b", 2)
	got, err := CollectModelEvidence(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	ref := `drop_index:public."` + idx + `"`
	for _, e := range got {
		if e.Kind == "action_log" && e.Ref == ref {
			if !strings.Contains(e.Detail, "2 times") ||
				!strings.Contains(e.Detail, "on table "+tableRef("public", table)) {
				t.Fatalf("evidence %+v, want 2 drops on table %s", e, table)
			}
			return
		}
	}
	t.Fatalf("no evidence %s in %+v", ref, got)
}
