package tuner

import (
	"context"
	"strings"
	"testing"
)

// No concurrent access tests: BuildDeleteSQL and InsertedHintText are pure
// string functions with no shared state.

func TestHintScope_DeleteCarriesHintText(t *testing.T) {
	sql := BuildDeleteSQL(17, "HashJoin(a b)")
	want := "DELETE FROM hint_plan.hints WHERE query_id = 17 " +
		"AND application_name = '' AND hints = $sageqh$HashJoin(a b)$sageqh$"
	if sql != want {
		t.Fatalf("BuildDeleteSQL = %q\nwant            %q", sql, want)
	}
}

// Nil/empty: an empty hint still yields a scoped DELETE that can match only
// an empty-text row, never an unscoped one.
func TestHintScope_DeleteEmptyHintStaysScoped(t *testing.T) {
	sql := BuildDeleteSQL(5, "")
	if !strings.HasSuffix(sql, "AND hints = $sageqh$$sageqh$") {
		t.Fatalf("empty hint delete is not scoped: %s", sql)
	}
}

// Invalid input: NUL bytes are stripped exactly as BuildInsertSQL strips
// them, so the DELETE matches the text the INSERT installed.
func TestHintScope_DeleteStripsNULsLikeInsert(t *testing.T) {
	raw := "Hash\x00Join(a b)"
	if sql := BuildDeleteSQL(3, raw); sql != BuildDeleteSQL(3, "HashJoin(a b)") {
		t.Fatalf("NUL not stripped: %q", sql)
	}
	got, ok := InsertedHintText(BuildInsertSQL(3, raw), 3)
	if !ok || got != "HashJoin(a b)" {
		t.Fatalf("InsertedHintText = %q, %t; want the stripped text", got, ok)
	}
}

func TestHintScope_DeleteRotatesCollidingTag(t *testing.T) {
	hint := "x $sageqh$ y $sageqh1$"
	sql := BuildDeleteSQL(9, hint)
	if !strings.HasSuffix(sql, "AND hints = $sageqh2$"+hint+"$sageqh2$") {
		t.Fatalf("tag not rotated past both collisions: %s", sql)
	}
}

func TestHintScope_InsertedHintTextRoundTrip(t *testing.T) {
	for _, hint := range []string{
		"HashJoin(a b)", `Set(work_mem "64MB")`, "a $sageqh$ b", "", "it's",
	} {
		got, ok := InsertedHintText(BuildInsertSQL(42, hint), 42)
		if !ok || got != hint {
			t.Errorf("InsertedHintText(%q) = %q, %t", hint, got, ok)
		}
	}
}

func TestHintScope_InsertedHintTextRejects(t *testing.T) {
	cases := map[string]struct {
		sql string
		qid int64
	}{
		"other query":  {BuildInsertSQL(42, "HashJoin(a b)"), 4},
		"prefix query": {BuildInsertSQL(421, "HashJoin(a b)"), 42},
		"empty":        {"", 42},
		"other sql":    {"ANALYZE public.t", 42},
		"delete":       {BuildDeleteSQL(42, "HashJoin(a b)"), 42},
		"unterminated": {"INSERT INTO hint_plan.hints (query_id, application_name, " +
			"hints) SELECT 42, '', $sageqh$HashJoin(a b)", 42},
		"no tag": {"INSERT INTO hint_plan.hints (query_id, application_name, " +
			"hints) SELECT 42, '', 'HashJoin(a b)'", 42},
	}
	for name, tc := range cases {
		if got, ok := InsertedHintText(tc.sql, tc.qid); ok {
			t.Errorf("%s: InsertedHintText = %q, true; want false", name, got)
		}
	}
}

// The rule-based finding's rollback is scoped to the very text its INSERT
// installs, so an action whose apply was a no-op over another row deletes
// nothing when it is rolled back.
func TestHintScope_BuildFindingRollbackMatchesInsert(t *testing.T) {
	tu := &Tuner{hintPlan: &HintPlanAvailability{Available: true, HintTableReady: true}}
	c := candidate{QueryID: 777, Query: "SELECT 1", MeanExecTime: 150, Calls: 100}
	hint := "IndexScan(orders orders_pkey)"
	f := tu.buildFinding(context.Background(), c, nil, hint, "t", "r")
	if f.RollbackSQL != BuildDeleteSQL(777, hint) {
		t.Fatalf("RollbackSQL = %q, want the delete scoped to %q", f.RollbackSQL, hint)
	}
	if got, ok := InsertedHintText(f.RecommendedSQL, 777); !ok || got != hint {
		t.Fatalf("InsertedHintText(RecommendedSQL) = %q, %t; want %q", got, ok, hint)
	}
}
