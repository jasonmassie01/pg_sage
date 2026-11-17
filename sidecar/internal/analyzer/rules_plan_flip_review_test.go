package analyzer

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// guard-02c review: real pg_stat_statements query ids are 64-bit hashes,
// beyond float64's exact integers, and the flipped plan stays the newest
// until the capture after pg_sage's re-ANALYZE arrives. No concurrent
// access tests in this file: the functions under test hold no shared state.

// bigQueryID is a typical pg_stat_statements queryid: float64 cannot hold it.
const bigQueryID int64 = -8_446_744_073_709_551_613

func TestPlanFlip_FollowUpKeepsA64BitQueryID(t *testing.T) {
	stored, err := json.Marshal(map[string]any{"queryid": bigQueryID,
		"relation": "public.orders", "previous_plan_hash": "v2:aaaa",
		"current_plan_hash": "v2:bbbb"})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := decodeFlipDetail(stored)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	row := openFlip{ID: 7, Ident: "queryid:-8446744073709551613", Status: "resolved",
		Severity: "warning", Title: "flip", Detail: detail, ActionID: 99,
		ActionAt: flipT0.Add(4 * time.Hour)}
	db := &fakeFlipDB{now: flipT0.Add(6 * time.Hour), open: []openFlip{row},
		latest: map[int64]*latestCapture{bigQueryID: capture("v2:bbbb", 5*time.Hour)}}
	a := flipAnalyzer(db)
	got := a.checkPlanFlips(context.Background(), nil, true)
	if len(got) != 1 || got[0].Detail[detailReanalyzeVerdict] != verdictNoChange {
		t.Fatalf("findings = %+v, want the handed-on flip of query %d", got, bigQueryID)
	}
	raw, err := json.Marshal(got[0].Detail)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		QueryID int64 `json:"queryid"`
	}
	if err := json.Unmarshal(raw, &back); err != nil || back.QueryID != bigQueryID {
		t.Fatalf("re-emitted queryid = %d (%v), want %d", back.QueryID, err, bigQueryID)
	}
}

func TestPlanFlip_FlipQueryID(t *testing.T) {
	cases := map[string]struct {
		ident string
		want  int64
		ok    bool
	}{
		"positive": {"queryid:42", 42, true},
		"64-bit":   {"queryid:-8446744073709551613", bigQueryID, true},
		"zero":     {"queryid:0", 0, false},
		"no id":    {"queryid:", 0, false},
		"garbage":  {"queryid:12x", 0, false},
		"overflow": {"queryid:99999999999999999999", 0, false},
		"other":    {"table:public.orders", 0, false},
		"empty":    {"", 0, false},
	}
	for name, tc := range cases {
		got, ok := flipQueryID(openFlip{Ident: tc.ident})
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: flipQueryID(%q) = %d, %v; want %d, %v", name, tc.ident, got, ok,
				tc.want, tc.ok)
		}
	}
}

// The judged blocks per call survive the exact decoding of the stored detail.
func TestPlanFlip_JudgeReadsDecodedBlocks(t *testing.T) {
	detail, err := decodeFlipDetail([]byte(`{"current_plan_hash": "v2:bbbb",
		"blocks_per_call_after": 4800}`))
	if err != nil {
		t.Fatal(err)
	}
	row := openFlip{Detail: detail, ActionID: 99, ActionAt: flipT0.Add(4 * time.Hour)}
	if got := judgeReanalyze(row, capture("v2:cccc", 5*time.Hour), fptr(9000)); got !=
		verdictNoChange {
		t.Fatalf("verdict = %q, want no_change (blocks per call rose)", got)
	}
	if got := judgeReanalyze(row, capture("v2:cccc", 5*time.Hour), fptr(10)); got !=
		verdictImproved {
		t.Fatalf("verdict = %q, want improved", got)
	}
	if _, err := decodeFlipDetail([]byte(`{"queryid":`)); err == nil {
		t.Fatal("a truncated detail decoded without an error")
	}
}

// The flip was caused by an autoanalyze between the captures, and pg_sage
// re-analyzed after the newest one: that plan predates the re-ANALYZE, so
// the flip is not re-emitted (it would reopen the row the action resolved
// before its verdict) and an open row of it is kept.
func TestPlanFlip_NewestPlanBeforePgSageReanalyzeAwaitsTheVerdict(t *testing.T) {
	reanalyzed := flipT0.Add(3 * time.Hour) // after CurrentAt (+2h)
	db := &fakeFlipDB{rels: ordersStats(&reanalyzed, flipAt(time.Hour)),
		sage: []sageAnalyze{{Target: "public.orders", At: reanalyzed}},
		now:  flipT0.Add(3*time.Hour + 5*time.Minute)}
	a := flipAnalyzer(db)
	if got := a.checkPlanFlips(context.Background(), flipPairs(t), true); len(got) != 0 {
		t.Fatalf("findings = %+v, want none until a plan after the re-ANALYZE", got)
	}
	if !a.eval.kept(categoryPlanFlip)["queryid:42"] {
		t.Fatal("the flip awaiting its verdict was not kept open")
	}
	if !a.eval.ok[categoryPlanFlip] || a.eval.hasFailed(categoryPlanFlip) {
		t.Fatal("the category was not evaluated")
	}
}

// pg_sage's ANALYZE before the newest capture (the flip came after it):
// the flip is reported, held without a second ANALYZE within 24 hours.
func TestPlanFlip_PgSageAnalyzeBeforeTheNewestPlanStillReports(t *testing.T) {
	earlier := flipT0.Add(-time.Hour)
	db := &fakeFlipDB{rels: ordersStats(&earlier, flipAt(time.Hour)),
		sage: []sageAnalyze{{Target: "public.orders", At: earlier}},
		now:  flipT0.Add(3 * time.Hour)}
	a := flipAnalyzer(db)
	got := a.checkPlanFlips(context.Background(), flipPairs(t), true)
	if len(got) != 1 || got[0].RecommendedSQL != "" ||
		got[0].Detail["reanalyze_held_until"] == nil {
		t.Fatalf("findings = %+v, want one held flip", got)
	}
}

// open -> re-analyzed -> no change on a real server with a 64-bit queryid.
func TestPlanFlipDB_ReanalyzeVerdictWithA64BitQueryID(t *testing.T) {
	f := newFlipFixture(t, 7)
	f.qid = bigQueryID
	f.flip(t)
	_, flips := f.run(t)
	if len(flips) != 1 {
		t.Fatalf("flips = %+v", flips)
	}
	f.reanalyzed(t, flips[0])
	f.capture(t)
	_, flips = f.run(t)
	if len(flips) != 1 || flips[0].Detail[detailReanalyzeVerdict] != verdictNoChange {
		t.Fatalf("after the re-analyze: %+v, want the handed-on flip", flips)
	}
}
