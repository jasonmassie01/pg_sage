package optimizer

import (
	"strings"
	"testing"
)

// review-02a: hiding both copies of (a) leaves the point lookup without an
// index: the set regresses past the bar, naming the statement.
func TestMeasureDropSet_HidingEveryAccessPathRegresses(t *testing.T) {
	f := newHideFixture(t)
	queries := []QueryInfo{{QueryID: 51, Calls: 100, TotalTimeMs: 100, Text: f.pointLookup}}
	set := []IndexRef{{Name: "items_a", OID: f.itemsA}, {Name: "items_a_dup", OID: f.itemsDup}}
	res, err := NewHypoPG(f.pool, noopLog2).MeasureDropSet(t.Context(), set, queries, 10)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res.Measured != 1 || res.Guarded != 1 || len(res.Regressions) != 1 ||
		res.Regressions[0].QueryID != 51 || res.Regressions[0].Pct <= 10 {
		t.Fatalf("result %+v, want queryid 51 regressed past 10%%", res)
	}
	if !strings.Contains(res.HiddenIndex, "items_a") ||
		!strings.Contains(res.HiddenIndex, "items_a_dup") {
		t.Fatalf("hidden %q, want both indexes named", res.HiddenIndex)
	}
	assertHypoPGSessionClean(t, f.pool)
	assertNothingHidden(t, f.pool)
}

// Hiding one copy leaves its twin: no regression.
func TestMeasureDropSet_ATwinLeftBehindKeepsTheLookup(t *testing.T) {
	f := newHideFixture(t)
	queries := []QueryInfo{{QueryID: 52, Calls: 100, TotalTimeMs: 100, Text: f.pointLookup}}
	set := []IndexRef{{Name: "items_a_dup", OID: f.itemsDup}}
	res, err := NewHypoPG(f.pool, noopLog2).MeasureDropSet(t.Context(), set, queries, 10)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res.Measured != 1 || len(res.Regressions) != 0 || res.OldIndexPresent {
		t.Fatalf("result %+v, want one measured statement and no regression", res)
	}
	assertNothingHidden(t, f.pool)
}

func TestMeasureDropSet_AnIndexThatIsGoneIsAnError(t *testing.T) {
	f := newHideFixture(t)
	queries := []QueryInfo{{QueryID: 53, Calls: 1, TotalTimeMs: 1, Text: f.pointLookup}}
	set := []IndexRef{{Name: "items_gone", OID: 4294967000}}
	_, err := NewHypoPG(f.pool, noopLog2).MeasureDropSet(t.Context(), set, queries, 10)
	if err == nil || !strings.Contains(err.Error(), "items_gone") {
		t.Fatalf("err = %v, want one naming items_gone", err)
	}
	assertNothingHidden(t, f.pool)
}
