package tuner

import (
	"context"
	"testing"
)

// tuning-09: nothing ever set a deterministic query rewrite, so the rewrite
// plumbing is gone. The sage.query_hints columns stay (tuning-11 reads
// them); the tuner writes '' so a stale value never survives a re-proposal.

func TestBuildFinding_NeverCarriesRewriteDetail(t *testing.T) {
	tu := New(nil, TunerConfig{}, nil, noopLog)
	c := candidate{QueryID: 7, Query: "SELECT 1"}
	f := tu.buildFinding(context.Background(), c,
		[]PlanSymptom{{Kind: SymptomBadNestedLoop}}, "HashJoin(a b)", "title", "why")
	for _, key := range []string{"suggested_rewrite", "rewrite_rationale"} {
		if _, ok := f.Detail[key]; ok {
			t.Errorf("Detail carries %s: %#v", key, f.Detail[key])
		}
	}
	if f.Detail["hint_directive"] != "HashJoin(a b)" || f.Recommendation != "why" {
		t.Fatalf("finding lost its hint or rationale: %+v", f)
	}
}

func TestUpsertQueryHint_ClearsStaleRewriteColumns(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, suggested_rewrite, rewrite_rationale, status)
		VALUES ($1, 'HashJoin(a b)', 'bad_nested_loop', 'SELECT stale', 'old', 'proposed')`,
		hintStatusQueryID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	tu := readyTuner(pool)
	tu.upsertQueryHint(ctx, hintStatusQueryID, `Set(work_mem "64MB")`, "disk_sort")
	assertRewriteColumnsEmpty(t, ctx, `Set(work_mem "64MB")`)

	if _, err := pool.Exec(ctx, "DELETE FROM sage.query_hints WHERE queryid = $1",
		hintStatusQueryID); err != nil {
		t.Fatalf("clear proposal: %v", err)
	}
	tu.upsertQueryHint(ctx, hintStatusQueryID, "HashJoin(a b)", "bad_nested_loop")
	assertRewriteColumnsEmpty(t, ctx, "HashJoin(a b)")
}

func assertRewriteColumnsEmpty(t *testing.T, ctx context.Context, wantHint string) {
	t.Helper()
	pool, _ := requireTunerDB(t)
	var hint, rewrite, rationale string
	err := pool.QueryRow(ctx, `SELECT hint_text, suggested_rewrite, rewrite_rationale
		FROM sage.query_hints WHERE queryid = $1 AND status = 'proposed'`,
		hintStatusQueryID).Scan(&hint, &rewrite, &rationale)
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if hint != wantHint || rewrite != "" || rationale != "" {
		t.Fatalf("proposal = (%q, %q, %q), want (%q, \"\", \"\")",
			hint, rewrite, rationale, wantHint)
	}
}
