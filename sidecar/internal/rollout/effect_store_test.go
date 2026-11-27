package rollout

import (
	"slices"
	"strings"
	"testing"
)

// No concurrent access tests: queryIDs is a pure function of its input.

// pg_stat_statements queryids span all of int64, so a target_queryids
// array the executor writes as JSON numbers must decode exactly: a float64
// round trip would turn them into other queries' ids.
func TestQueryIDsDecodeLargeIDsExactly(t *testing.T) {
	raw := []byte(`[-3420843250382719386, 9007199254740993, "8846219947185524417", 11]`)
	got, err := queryIDs(raw)
	want := []int64{-3420843250382719386, 9007199254740993, 8846219947185524417, 11}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("queryIDs = %v (%v), want %v", got, err, want)
	}
}

func TestQueryIDsEmptyAndNull(t *testing.T) {
	for _, raw := range []string{"", "null", "[]"} {
		got, err := queryIDs([]byte(raw))
		if err != nil || len(got) != 0 {
			t.Fatalf("queryIDs(%q) = %v (%v), want none", raw, got, err)
		}
	}
}

func TestQueryIDsRejectNonIntegers(t *testing.T) {
	for raw, want := range map[string]string{
		`{"a": 1}`:     "not a JSON array",
		`[1.5]`:        "is not a number",
		`["abc"]`:      `"abc" is not a number`,
		`[true]`:       "is not a number",
		`[1e30]`:       "is not a number",
		`[1, [2]]`:     "is not a number",
		`[9.0e18, 1]`:  "is not a number",
		`["1", null]`:  "is not a number",
		`[-1, "-2.0"]`: `"-2.0" is not a number`,
	} {
		got, err := queryIDs([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("queryIDs(%s) = %v (%v), want an error with %q", raw, got, err, want)
		}
	}
}

// A measured v2 effect whose target queries could not be read still
// decides, with no fleet control, and the detail says why.
func TestEffectMeasuredWithUnreadTargetsSaysWhy(t *testing.T) {
	a, b, c := newEffectTarget("a", 12, 12), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 0, 0)
	a.effect.Queries, a.effect.Reason = nil, `target queryid "x" is not a number`
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.EffectMethod != EffectOwnV2 || !near(o.EffectPct, 12) || o.RegressionPct != 12 ||
		o.ControlPct != nil || !strings.Contains(o.Detail, `"x" is not a number`) {
		t.Fatalf("outcome = %+v", o)
	}
}
