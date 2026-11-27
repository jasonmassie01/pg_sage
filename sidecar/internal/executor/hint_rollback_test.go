package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/tuner"
)

// legacyHintDelete is the rollback SQL tuner.BuildDeleteSQL produced before
// tuning-02a: scoped to the query but not to pg_sage's hint text.
func legacyHintDelete(qid string) string {
	return "DELETE FROM hint_plan.hints WHERE query_id = " + qid +
		" AND application_name = ''"
}

func TestHintRollback_LegacyPatternRecognized(t *testing.T) {
	for sql, want := range map[string]int64{
		legacyHintDelete("123"):               123,
		legacyHintDelete("-77"):               -77,
		"  " + legacyHintDelete("5") + " ;\n": 5,
		"delete from HINT_PLAN.HINTS where query_id=9 and application_name=''": 9,
		"/* pg_sage */ " + legacyHintDelete("11"):                              11,
	} {
		got, ok := legacyHintDeleteQueryID(sql)
		if !ok || got != want {
			t.Errorf("legacyHintDeleteQueryID(%q) = %d, %t; want %d", sql, got, ok, want)
		}
	}
}

func TestHintRollback_NonLegacyLeftAlone(t *testing.T) {
	for _, sql := range []string{
		tuner.BuildDeleteSQL(123, "HashJoin(a b)"),
		legacyHintDelete("123") + " AND hints = 'x'",
		"DELETE FROM hint_plan.hints WHERE query_id = 1",
		legacyHintDelete("99999999999999999999999"), // overflows int64
		"DROP INDEX CONCURRENTLY public.idx",
		"",
	} {
		if qid, ok := legacyHintDeleteQueryID(sql); ok {
			t.Errorf("legacyHintDeleteQueryID(%q) = %d, true; want false", sql, qid)
		}
	}
}

// Non-hint rollbacks pass through without touching the database (nil pool).
func TestHintRollback_ScopeLeavesOtherSQL(t *testing.T) {
	scoped := tuner.BuildDeleteSQL(8, "HashJoin(a b)")
	for _, sql := range []string{"DROP INDEX CONCURRENTLY public.idx_a", scoped} {
		got, err := scopeHintRollback(context.Background(), nil, 1, sql)
		if err != nil || got != sql {
			t.Errorf("scopeHintRollback(%q) = %q, %v; want unchanged", sql, got, err)
		}
	}
}

// Error propagation: a legacy rollback with no database to read the
// recorded text from is refused, never run unscoped.
func TestHintRollback_ScopeWithoutPoolRefuses(t *testing.T) {
	got, err := scopeHintRollback(context.Background(), nil, 1, legacyHintDelete("8"))
	if !errors.Is(err, ErrHintTextUnknown) || got != "" {
		t.Fatalf("scopeHintRollback = %q, %v; want ErrHintTextUnknown", got, err)
	}
}

func TestHintRollback_ChooseRecordedText(t *testing.T) {
	own := tuner.BuildInsertSQL(7, "HashJoin(a b)")
	cases := []struct {
		name     string
		executed string
		recorded []string
		want     string
		ok       bool
	}{
		{"executed text recorded", own, []string{"NestLoop(a b)", "HashJoin(a b)"},
			"HashJoin(a b)", true},
		{"executed text not recorded", own, []string{"NestLoop(a b)"}, "", false},
		{"nothing recorded", own, nil, "", false},
		{"unparsed, one recorded", "INSERT INTO hint_plan.hints VALUES (7)",
			[]string{"NestLoop(a b)"}, "NestLoop(a b)", true},
		{"unparsed, two recorded", "", []string{"A", "B"}, "", false},
		{"unparsed, none recorded", "", nil, "", false},
	}
	for _, tc := range cases {
		got, ok := chooseRecordedHint(tc.executed, 7, tc.recorded)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: chooseRecordedHint = %q, %t; want %q, %t",
				tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
