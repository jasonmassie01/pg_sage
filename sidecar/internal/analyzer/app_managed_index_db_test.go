package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// facts-03: pg_sage has quoted its own drops since ab94415e
// (DROP INDEX CONCURRENTLY "public"."idx";). The drop history must key
// each index by its lower-cased catalog schema.name, the form
// markAppManaged looks findings up by, whatever the drop's quoting.

// No concurrent-access tests: loadAppManagedIndexes is a stateless read
// that shares nothing between calls.

// managedTable creates a uniquely named table in public, dropped (with its
// drop history) when the test ends.
func managedTable(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	tbl := fmt.Sprintf("amq_%d", time.Now().UnixNano())
	preflightSQL(t, p, fmt.Sprintf("CREATE TABLE public.%s (a int, b int)", tbl))
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "DROP TABLE IF EXISTS public."+tbl)
		_, _ = p.Exec(context.Background(),
			"DELETE FROM sage.action_log WHERE rollback_sql LIKE $1", "%"+tbl+"%")
	})
	return tbl
}

// recordDrop writes one executed drop of index (its exact catalog name)
// into the action log, with the SQL pg_sage's rules generate, and returns
// the index's definition as rollback.
func recordDrop(t *testing.T, ctx context.Context, p *pgxpool.Pool, sql, index string) {
	t.Helper()
	var def string
	if err := p.QueryRow(ctx, "SELECT pg_get_indexdef(to_regclass($1))",
		sanitize.QuoteQualifiedName("public", index)).Scan(&def); err != nil {
		t.Fatalf("definition of %q: %v", index, err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		rollback_sql, outcome, executed_at)
		VALUES ('drop_index', $1, $2, 'success', now() - interval '1 day')`,
		sql, def); err != nil {
		t.Fatalf("record drop of %q: %v", index, err)
	}
}

// The regression: a quoted lower-case drop row ("public"."idx_x") and an
// EF Core-style mixed-case one fold the duplicate and unused findings for
// those indexes into app_managed_index. Fails on 61152ef3, which keyed
// the history on lower('"public"."idx_x"').
func TestAppManagedQuotedDropsFoldDropFindings(t *testing.T) {
	p, ctx := preflightPool(t)
	tbl := managedTable(t, p)
	lowerIdx, efIdx := "idx_x_"+tbl, "IX_Orders_A_"+tbl
	preflightSQL(t, p, fmt.Sprintf(`CREATE INDEX %s ON public.%s (a);
		CREATE INDEX "%s" ON public.%s (b)`, lowerIdx, tbl, efIdx, tbl))
	for _, idx := range []string{lowerIdx, efIdx} {
		recordDrop(t, ctx, p, dropIndexSQL(collector.IndexStats{SchemaName: "public",
			IndexRelName: idx}), idx)
	}
	got, err := loadAppManagedIndexes(ctx, p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, idx := range []string{lowerIdx, efIdx} {
		m, ok := got[strings.ToLower("public."+idx)]
		if !ok || m.Drops != 1 || m.Schema != "public" || m.Name != idx ||
			m.Table != tbl || !m.Qualified {
			t.Fatalf("history of %s = %+v (%v), want one qualified drop in the catalog's "+
				"case on table %s; keys %v", idx, m, ok, tbl, keysOf(got))
		}
	}
	in := []Finding{dropFinding("unused_index", "public."+lowerIdx),
		dropFinding("duplicate_index", "public."+efIdx)}
	out := markAppManaged(in, got)
	if len(out) != 2 {
		t.Fatalf("findings = %+v, want one app_managed_index per index", out)
	}
	for i, idx := range []string{lowerIdx, efIdx} {
		if out[i].Category != CategoryAppManagedIndex || out[i].RecommendedSQL != "" ||
			out[i].ObjectIdentifier != "public."+idx {
			t.Fatalf("finding %d = %+v, want %s folded into %s", i, out[i], idx,
				CategoryAppManagedIndex)
		}
	}
}

func keysOf(m map[string]appManagedIndex) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Invalid input: names with a space, a dot or a double quote are read
// whole from the quoted drop text, and drop text that is no name at all
// (an unterminated quote, an empty quoted name) neither matches an index
// nor fails the read (PG14 and PG15 raise on to_regclass of bad syntax).
func TestAppManagedOddNamesReadWhole(t *testing.T) {
	p, ctx := preflightPool(t)
	tbl := managedTable(t, p)
	odd := []string{"my idx " + tbl, "a.b " + tbl, `q"x ` + tbl}
	for i, idx := range odd {
		col := []string{"a", "b", "a, b"}[i]
		preflightSQL(t, p, fmt.Sprintf("CREATE INDEX %s ON public.%s (%s)",
			sanitize.QuoteIdentifier(idx), tbl, col))
		recordDrop(t, ctx, p, dropIndexSQL(collector.IndexStats{SchemaName: "public",
			IndexRelName: idx}), idx)
	}
	for _, junk := range []string{`DROP INDEX CONCURRENTLY "public"."unterminated`,
		`DROP INDEX ""`, `DROP INDEX public.""`} {
		if _, err := p.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
			rollback_sql, outcome) VALUES ('drop_index', $1, $2, 'success')`, junk,
			"CREATE INDEX junk ON public."+tbl+" (a)"); err != nil {
			t.Fatalf("record %q: %v", junk, err)
		}
	}
	got, err := loadAppManagedIndexes(ctx, p)
	if err != nil {
		t.Fatalf("odd drop text failed the whole read: %v", err)
	}
	for _, idx := range odd {
		m, ok := got[strings.ToLower("public."+idx)]
		if !ok || m.Name != idx || m.Schema != "public" || m.Drops != 1 {
			t.Errorf("index %q: history %+v (%v); keys %v", idx, m, ok, keysOf(got))
		}
	}
}

// Nil/empty: an index pg_sage dropped that is still gone has no history
// entry, whether the drop was quoted or not.
func TestAppManagedGoneIndexHasNoHistory(t *testing.T) {
	p, ctx := preflightPool(t)
	tbl := managedTable(t, p)
	gone := "IX_Gone_" + tbl
	preflightSQL(t, p, fmt.Sprintf(`CREATE INDEX "%s" ON public.%s (a)`, gone, tbl))
	recordDrop(t, ctx, p, dropIndexSQL(collector.IndexStats{SchemaName: "public",
		IndexRelName: gone}), gone)
	preflightSQL(t, p, fmt.Sprintf(`DROP INDEX public."%s"`, gone))
	got, err := loadAppManagedIndexes(ctx, p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m, ok := got[strings.ToLower("public."+gone)]; ok {
		t.Fatalf("a dropped index that stayed gone is app-managed: %+v", m)
	}
}

// Error propagation: a read that cannot reach the history says what it
// was reading.
func TestAppManagedReadErrorNamesTheHistory(t *testing.T) {
	p, _ := preflightPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadAppManagedIndexes(ctx, p); err == nil ||
		!strings.Contains(err.Error(), "read index drop history") {
		t.Fatalf("cancelled read error = %v, want it to name the drop history", err)
	}
}

// dropNamesSchema reports whether DROP INDEX text names the index's
// schema: a dot outside double quotes.
func TestDropNamesSchema(t *testing.T) {
	cases := map[string]bool{
		"public.idx": true, `"public"."IX_A"`: true, `public."a.b"`: true,
		"idx": false, `"a.b"`: false, `"a"".b"`: false, "": false, `"a""".b`: true,
	}
	for in, want := range cases {
		if got := dropNamesSchema(in); got != want {
			t.Errorf("dropNamesSchema(%q) = %v, want %v", in, got, want)
		}
	}
}
