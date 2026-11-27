package fleetlearn

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedOpenFindings inserts n open findings with distinct objects.
func seedOpenFindings(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	exec(t, pool, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, status)
		SELECT 'missing_index', 'warning', 'table', 'public.t' || g || '|btree(a)',
		'Missing index', '{}', 'open' FROM generate_series(1, $1) g`, n)
}

func TestReadOpenFindings_TruncationBoundary(t *testing.T) {
	ctx := context.Background()
	big, exact := freshDB(t, "trunc_big"), freshDB(t, "trunc_exact")
	seedOpenFindings(t, big, 501)
	seedOpenFindings(t, exact, 500)
	got, truncated, err := readOpenFindings(ctx, Source{Name: "big", Pool: big}, 500)
	if err != nil || len(got) != 500 || !truncated {
		t.Fatalf("501 open: rows=%d truncated=%v err=%v, want 500 and truncated",
			len(got), truncated, err)
	}
	got, truncated, err = readOpenFindings(ctx, Source{Name: "exact", Pool: exact}, 500)
	if err != nil || len(got) != 500 || truncated {
		t.Fatalf("500 open: rows=%d truncated=%v err=%v, want 500 and not truncated",
			len(got), truncated, err)
	}
	res := NewFindingsReader().Collect(ctx, []Source{{Name: "big", Pool: big},
		{Name: "exact", Pool: exact}}, 2, 500)
	if len(res.Truncated) != 1 || res.Truncated[0] != "big" {
		t.Fatalf("truncated = %v, want [big]", res.Truncated)
	}
	if len(res.Findings) != 500 {
		t.Fatalf("findings = %d, want the 500 objects both read", len(res.Findings))
	}
	for _, f := range res.Findings {
		if f.Databases != 2 {
			t.Fatalf("finding %s on %d databases, want 2 (big capped at 500)",
				f.Key, f.Databases)
		}
	}
}

func TestFindingsReader_GroupsTheSameFindingOnTwoDatabases(t *testing.T) {
	ctx := context.Background()
	one, two := freshDB(t, "group_one"), freshDB(t, "group_two")
	for _, p := range []*pgxpool.Pool{one, two} {
		seedOpenFindings(t, p, 1)
	}
	res := NewFindingsReader().Collect(ctx, []Source{{Name: "one", Pool: one},
		{Name: "two", Pool: two}}, 2, 500)
	if res.DatabasesScanned != 2 || len(res.Errors) != 0 || len(res.Truncated) != 0 {
		t.Fatalf("result = %+v, want both read cleanly", res)
	}
	if len(res.Findings) != 1 || res.Findings[0].Databases != 2 {
		t.Fatalf("findings = %+v, want one item on both databases", res.Findings)
	}
	occ := res.Findings[0].Occurrences
	if len(occ) != 2 || occ[0].Database != "one" || occ[1].Database != "two" ||
		occ[0].ID == 0 {
		t.Fatalf("occurrences = %+v, want one per database with ids", occ)
	}
}
