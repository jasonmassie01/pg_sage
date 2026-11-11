package tuner

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tuning-05 state transition: revalidation's counterfactual. At
// reverify_due an active hint that still changes the plan stays (its costs
// recorded); once its index is replaced so the hint no longer changes the
// plan, it is retired with reason no_effect and a removal finding follows.

const hxItemsQuery = `SELECT * FROM hx.items i WHERE b IS NOT NULL`

func hxItemsQueryID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE hx.items (a int, b int, pad text)`,
		`INSERT INTO hx.items SELECT g, (g * 7919) % 50000, repeat('x', 20)
		   FROM generate_series(1, 50000) g`,
		`CREATE INDEX hx_items_b_idx ON hx.items (b)`,
		`ANALYZE hx.items`,
		hxItemsQuery,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("items fixture %q: %v", stmt, err)
		}
	}
	return queryIDOf(t, pool, hxItemsQuery)
}

type hintRowState struct {
	status             string
	before, after      *float64
	verified, reverify bool
}

func readHintRow(t *testing.T, pool *pgxpool.Pool, qid int64) hintRowState {
	t.Helper()
	var s hintRowState
	if err := pool.QueryRow(context.Background(), `SELECT status, before_cost, after_cost,
		verified_at IS NOT NULL, reverify_due IS NOT NULL
		FROM sage.query_hints WHERE queryid = $1 ORDER BY id DESC LIMIT 1`, qid).Scan(
		&s.status, &s.before, &s.after, &s.verified, &s.reverify); err != nil {
		t.Fatalf("read hint row: %v", err)
	}
	return s
}

func TestHintExplainRevalidate_RetiresHintWithNoEffect(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	qid := hxItemsQueryID(t, pool)
	tu := gatedTuner(t, pool, qid)
	const hint = "IndexScan(i hx_items_b_idx)"
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status, reverify_due) VALUES ($1, $2, 's', 'active',
		now())`, qid, hint); err != nil {
		t.Fatal(err)
	}
	installHintRow(t, pool, qid, "", hint)

	rpt, err := tu.Revalidate(ctx)
	if err != nil || rpt.Retired != 0 || rpt.Errors != 0 {
		t.Fatalf("first pass: %+v, %v", rpt, err)
	}
	s := readHintRow(t, pool, qid)
	if s.status != "active" && s.status != "dormant" || !s.verified || s.before == nil ||
		s.after == nil || *s.after == *s.before {
		t.Fatalf("a working hint must stay live with its costs: %+v", s)
	}

	for _, stmt := range []string{`DROP INDEX hx.hx_items_b_idx`,
		`CREATE INDEX hx_items_b_idx ON hx.items (a)`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	rpt, err = tu.Revalidate(ctx)
	if err != nil || rpt.Retired != 1 || rpt.Broken != 0 {
		t.Fatalf("after the index was replaced: %+v, %v", rpt, err)
	}
	if s := readHintRow(t, pool, qid); s.status != "retired" {
		t.Fatalf("status %q, want retired", s.status)
	}
	var found bool
	for _, f := range tu.hintRemovalFindings(ctx) {
		if f.ObjectIdentifier == "queryid:"+strconv.FormatInt(qid, 10) &&
			f.Detail["hint_status"] == "retired" && f.RecommendedSQL != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("no removal finding for the retired, still installed hint")
	}
}

// State: a hint not yet due is never planned (no costs recorded), and with
// the gate off a due hint is left as it was.
func TestHintExplainRevalidate_OnlyDueHintsAndGateOn(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	qid := hxItemsQueryID(t, pool)
	tu := gatedTuner(t, pool, qid)
	tu.cfg.HintRetirementDays = 0
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES ($1, 'IndexScan(i hx_items_b_idx)', 's',
		'active')`, qid); err != nil {
		t.Fatal(err)
	}
	if _, err := tu.Revalidate(ctx); err != nil {
		t.Fatal(err)
	}
	if s := readHintRow(t, pool, qid); s.verified || s.status == "retired" {
		t.Fatalf("a hint not due was re-verified: %+v", s)
	}
	off := New(pool, tu.cfg, tu.hints(), noopLogFn)
	if _, err := pool.Exec(ctx, `UPDATE sage.query_hints SET reverify_due = now()
		WHERE queryid = $1`, qid); err != nil {
		t.Fatal(err)
	}
	if rpt, err := off.Revalidate(ctx); err != nil || rpt.Retired != 0 {
		t.Fatalf("gate off: %+v, %v", rpt, err)
	}
	if s := readHintRow(t, pool, qid); s.verified || s.status == "retired" {
		t.Fatalf("gate off re-verified the hint: %+v", s)
	}
}
