package cases_test

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// indexOnly is the executor's contract for a concurrent index build only.
func indexOnly(sql string) (string, string) {
	if strings.Contains(sql, "CONCURRENTLY") {
		return "create_index_concurrently", "moderate"
	}
	return "", ""
}

// Newer findings the executor runs itself must not crowd an older
// migration out of a pass: the limit counts candidates, not rows read.
func TestPRModeDB_LoadCandidatesIsNotStarvedByExecutorFindings(t *testing.T) {
	pool := prModePool(t)
	ctx := context.Background()
	id := migrationFinding(t, pool)
	_, err := pool.Exec(ctx, `UPDATE sage.findings SET last_seen = now() - interval '1 day'
	    WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
	    object_identifier, title, detail, recommended_sql)
	    SELECT 'missing_index', 'warning', 'table', 'public.t' || g, 'index ' || g, '{}',
	      'CREATE INDEX CONCURRENTLY i' || g || ' ON public.t' || g || ' (a)'
	    FROM generate_series(1, 40) g`)
	require.NoError(t, err)
	got, err := cases.LoadPRCandidates(ctx, pool, indexOnly, 5)
	require.NoError(t, err)
	require.Equal(t, 1, len(got), "the only candidate, behind 40 newer executor findings")
	require.Equal(t, id, got[0].FindingID)

	_, err = pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
	    object_identifier, title, detail, recommended_sql)
	    SELECT 'migration_safety', 'warning', 'table', 'public.m' || g, 'check ' || g, '{}',
	      'ALTER TABLE public.m' || g || ' ADD COLUMN c int'
	    FROM generate_series(1, 8) g`)
	require.NoError(t, err)
	capped, err := cases.LoadPRCandidates(ctx, pool, indexOnly, 3)
	require.NoError(t, err)
	require.Equal(t, 3, len(capped), "at most limit candidates")
}
