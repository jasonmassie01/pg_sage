package facts

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// targetTables are the relations createTargetTables makes: only is in
// public alone, shared in public and the test schema, hidden in the test
// schema alone (never on the search_path).
type targetTables struct{ testSchema, only, shared, hidden string }

func createTargetTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) targetTables {
	t.Helper()
	tt := targetTables{testSchema: uniqueName("test_a_"), only: uniqueName("orders_"),
		shared: uniqueName("shared_"), hidden: uniqueName("hidden_")}
	qs := pgx.Identifier{tt.testSchema}.Sanitize()
	execAll(t, ctx, pool, "CREATE SCHEMA "+qs,
		"CREATE TABLE public."+tt.only+" (id int)",
		"CREATE TABLE public."+tt.shared+" (id int)",
		"CREATE TABLE "+qs+"."+tt.shared+" (id int)",
		"CREATE TABLE "+qs+"."+tt.hidden+" (id int)")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DROP SCHEMA IF EXISTS "+qs+" CASCADE")
		_, _ = pool.Exec(bg, "DROP TABLE IF EXISTS public."+tt.only+", public."+tt.shared)
	})
	return tt
}

// facts-01 on real PostgreSQL: unqualified targets are qualified from the
// catalog, whatever the search_path; system and sage relations and names
// no user schema has are absent.
func TestCatalogResolverQualifiesTargetsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	tt := createTargetTables(t, ctx, pool)
	got, err := NewCatalogResolver(pool).TargetSchemas(ctx,
		[]string{tt.only, tt.shared, tt.hidden, "pg_class", "facts", "instance"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{tt.only: {"public"},
		tt.shared: {"public", tt.testSchema}, tt.hidden: {tt.testSchema}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("target schemas\n got %v\nwant %v", got, want)
	}
}

// A confirmed test_* fixture fact binds an unqualified target only through
// a test schema that has it.
func TestCatalogBinderFixtureFactBindsOnlyTestSchemaTargets(t *testing.T) {
	pool, ctx := livePool(t)
	tt := createTargetTables(t, ctx, pool)
	s := NewStore(pool)
	if _, err := s.Declare(ctx, operatorProposal(TypeTestFixture, KindSchema, "test_*"),
		"op", ""); err != nil {
		t.Fatal(err)
	}
	b := NewBinder(s, NewCatalogResolver(pool), nil)
	bind := func(target string) []policy.FactBinding {
		t.Helper()
		out, err := b.Bind(ctx, configRequest("ALTER SYSTEM SET work_mem='64MB'", target))
		if err != nil {
			t.Fatalf("bind %q: %v", target, err)
		}
		return out
	}
	for _, target := range []string{tt.only, "instance", "pg_class"} {
		if out := bind(target); out != nil {
			t.Fatalf("target %q must not bind: %+v", target, out)
		}
	}
	for _, target := range []string{tt.shared, tt.hidden} {
		out := bind(target)
		if len(out) != 1 || out[0].Object != tt.testSchema+"."+target ||
			out[0].Route != string(RouteExcluded) {
			t.Fatalf("target %q must bind through %s: %+v", target, tt.testSchema, out)
		}
	}
}
