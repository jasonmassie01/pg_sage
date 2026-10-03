package lint

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// D_catalog P6 / static.md F12: the TOAST rule called
// pg_total_relation_size up to three times per table over the whole
// catalog (551 ms on lifeos) and opened every relation in the pool
// backend each hour. It now reads relpages and returns at most 200 rows.

func TestToastHeavySQL_NoPerRelationSizeCalls(t *testing.T) {
	q := toastHeavyQuery("'pg_catalog'")
	for _, bad := range []string{"pg_total_relation_size(", "pg_relation_size(",
		"pg_table_size("} {
		if strings.Contains(q, bad) {
			t.Errorf("toast-heavy SQL calls %s per table", bad)
		}
	}
	if !strings.Contains(q, "LIMIT 200") || !strings.Contains(q, "/* pg_sage") {
		t.Errorf("toast-heavy SQL must be bounded and tagged:\n%s", q)
	}
}

func TestIntegration_ToastHeavy_FromRelpages(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %[1]s.heavy (id int PRIMARY KEY, payload text);
		INSERT INTO %[1]s.heavy SELECT g, (SELECT string_agg(md5(g::text || r::text), '')
			FROM generate_series(1, 300) r) FROM generate_series(1, 200) g;
		CREATE TABLE %[1]s.light (id int PRIMARY KEY, note text);
		INSERT INTO %[1]s.light SELECT g, 'n' FROM generate_series(1, 2000) g`, schema))
	require.NoError(t, err)
	for _, tbl := range []string{"heavy", "light"} {
		_, err := pool.Exec(ctx, fmt.Sprintf("VACUUM ANALYZE %s.%s", schema, tbl))
		require.NoError(t, err)
	}
	findings, err := (&ruleToastHeavy{}).Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)
	heavy := findingForTable(findings, schema, "heavy")
	require.NotNil(t, heavy, "a table whose TOAST is most of its size must be reported")
	require.Greater(t, heavy.TableSize, int64(1<<20))
	require.Nil(t, findingForTable(findings, schema, "light"))
	var exact int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT pg_total_relation_size($1::regclass)",
		schema+".heavy").Scan(&exact))
	require.LessOrEqual(t, heavy.TableSize, exact)
	require.Greater(t, heavy.TableSize, exact*8/10)
}
