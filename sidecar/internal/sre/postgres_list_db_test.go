package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// sre-09: the Cases page matched cases to the newest 100 investigations
// of a database, so a case whose investigation was older showed none.
// ListFilter.CaseIDs returns the newest investigation of each named case,
// one bounded probe of idx_sre_investigations_case per id, whatever the
// age of the investigation.

// seedLatestPerCase gives scope 97 cases of four investigations each, one
// case with two newest rows at the same created_at, and 3,000 newer
// investigations of other cases, so every named case is older than the
// newest 100.
func seedLatestPerCase(t *testing.T, pool *pgxpool.Pool, scope Scope) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{`INSERT INTO sage.sre_investigations (deployment_id,
		database_id, id, source_case_id, trigger_kind, trigger_fingerprint, state,
		created_at, updated_at, expires_at)
		SELECT $1::uuid, $2::uuid, gen_random_uuid(), 'case:latest:' || (g % 97),
		       'lock_blocking', sha256(('latest-' || g)::bytea), 'concluded',
		       now() - interval '1 day' - g * interval '1 minute',
		       now() - interval '1 day', now() + interval '1 day'
		FROM generate_series(0, 387) g`,
		`INSERT INTO sage.sre_investigations (deployment_id,
		database_id, id, source_case_id, trigger_kind, trigger_fingerprint, state,
		created_at, updated_at, expires_at)
		SELECT $1::uuid, $2::uuid, gen_random_uuid(), 'case:tie', 'lock_blocking',
		       sha256(('tie-' || g)::bytea), 'concluded',
		       now() - interval '2 days', now(), now() + interval '1 day'
		FROM generate_series(1, 2) g`,
		`INSERT INTO sage.sre_investigations (deployment_id,
		database_id, id, source_case_id, trigger_kind, trigger_fingerprint, state,
		created_at, updated_at, expires_at)
		SELECT $1::uuid, $2::uuid, gen_random_uuid(), 'case:noise:' || (g % 300),
		       'lock_blocking', sha256(('noise-' || g)::bytea), 'concluded',
		       now() - g * interval '1 second', now(), now() + interval '1 day'
		FROM generate_series(1, 3000) g`,
		`ANALYZE sage.sre_investigations`,
	}
	for _, q := range stmts {
		args := []any{string(scope.DeploymentID), string(scope.DatabaseID)}
		if q == `ANALYZE sage.sre_investigations` {
			args = nil
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.sre_investigations
			WHERE deployment_id = $1 AND database_id = $2`, string(scope.DeploymentID),
			string(scope.DatabaseID))
	})
}

// newestByCase is the expected answer, computed independently.
func newestByCase(t *testing.T, pool *pgxpool.Pool, scope Scope) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT DISTINCT ON (source_case_id)
		source_case_id, id::text FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2
		ORDER BY source_case_id, created_at DESC, id DESC`,
		string(scope.DeploymentID), string(scope.DatabaseID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, id string
		if err := rows.Scan(&c, &id); err != nil {
			t.Fatal(err)
		}
		out[c] = id
	}
	return out
}

func latestPerCaseIDs() []string {
	ids := []string{"case:tie", "case:none:1", "case:none:2", "case:none:3"}
	for i := 0; i < 96; i++ {
		ids = append(ids, fmt.Sprintf("case:latest:%d", i))
	}
	return ids
}

func TestLatestPerCase_ReturnsTheNewestOfEachNamedCase(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	seedLatestPerCase(t, pool, scope)
	want := newestByCase(t, pool, scope)
	ids := latestPerCaseIDs()
	if len(ids) != MaxListCaseIDs {
		t.Fatalf("test names %d ids, want %d", len(ids), MaxListCaseIDs)
	}
	page, err := st.List(ctx, scope, ListFilter{CaseIDs: ids})
	if err != nil {
		t.Fatalf("List(CaseIDs): %v", err)
	}
	if len(page.Items) != 97 || page.NextCursor != "" {
		t.Fatalf("got %d items, cursor %q; want 97 (3 cases have none), no cursor",
			len(page.Items), page.NextCursor)
	}
	seen := map[string]bool{}
	for i, inv := range page.Items {
		if seen[inv.CaseID] {
			t.Fatalf("case %s listed twice", inv.CaseID)
		}
		seen[inv.CaseID] = true
		if want[inv.CaseID] != string(inv.ID) {
			t.Fatalf("case %s: got %s, want its newest %s", inv.CaseID, inv.ID,
				want[inv.CaseID])
		}
		if i > 0 && newerThan(inv, page.Items[i-1]) {
			t.Fatalf("items %d and %d are not newest first", i-1, i)
		}
	}
	for _, id := range []string{"case:none:1", "case:none:2", "case:none:3"} {
		if seen[id] {
			t.Fatalf("case %s has no investigation but was listed", id)
		}
	}
}

func newerThan(a, b Investigation) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID > b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func TestLatestPerCase_StaysInsideTheScope(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	mine, other := testScope(t, ctx, st), testScope(t, ctx, st)
	seedLatestPerCase(t, pool, other)
	page, err := st.List(ctx, mine, ListFilter{CaseIDs: []string{"case:tie",
		"case:latest:1"}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("another database's cases: %d items (%v), want none",
			len(page.Items), err)
	}
}

func TestLatestPerCase_RejectsBadFiltersBeforeQuerying(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	for name, f := range map[string]ListFilter{
		"cursor":  {CaseIDs: []string{"a", "b"}, Cursor: "x"},
		"case id": {CaseIDs: []string{"a", "b"}, CaseID: "a"},
		"101 ids": {CaseIDs: caseIDs(MaxListCaseIDs + 1)},
	} {
		if _, err := st.List(ctx, scope, f); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// The statement probes the case index once per id: no scan and no sort of
// the scope's 3,390 investigations (an Incremental Sort included), and
// one row read per id.
func TestLatestPerCase_IsOneIndexProbePerCase(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	seedLatestPerCase(t, pool, scope)
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
	if _, err := tst.List(ctx, scope, ListFilter{CaseIDs: latestPerCaseIDs()}); err != nil {
		t.Fatal(err)
	}
	stmts := rec.Matching("unnest(", "FROM sage.sre_investigations")
	if len(stmts) != 1 {
		t.Fatalf("latest-per-case statements = %d, want 1", len(stmts))
	}
	plan, err := testdb.Explain(ctx, pool, "ANALYZE", stmts[0].SQL, stmts[0].Args...)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeqScans("sre_investigations") != 0 ||
		plan.Has(func(n testdb.PlanNode) bool { return strings.HasSuffix(n.NodeType, "Sort") }) {
		t.Fatalf("latest-per-case scans or sorts the scope:\n%s", plan)
	}
	var read float64
	plan.Walk(func(n testdb.PlanNode) {
		if n.Relation == "sre_investigations" {
			read += n.ActualRows * n.ActualLoops
		}
	})
	if read > 100 {
		t.Fatalf("100 case probes read %v investigations:\n%s", read, plan)
	}
}

// A case named twice (a repeated case_id) is one probe and one row.
func TestLatestPerCase_ListsARepeatedCaseOnce(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	seedLatestPerCase(t, pool, scope)
	want := newestByCase(t, pool, scope)
	page, err := st.List(ctx, scope, ListFilter{CaseIDs: []string{"case:latest:5",
		"case:tie", "case:latest:5"}})
	if err != nil {
		t.Fatalf("List(repeated CaseIDs): %v", err)
	}
	got := map[string]string{}
	for _, inv := range page.Items {
		got[inv.CaseID] = string(inv.ID)
	}
	if len(page.Items) != 2 || got["case:latest:5"] != want["case:latest:5"] ||
		got["case:tie"] != want["case:tie"] {
		t.Fatalf("repeated ids = %d items %v, want case:latest:5 and case:tie once each",
			len(page.Items), got)
	}
}
