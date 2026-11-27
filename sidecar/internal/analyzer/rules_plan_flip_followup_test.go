package analyzer

import (
	"context"
	"strings"
	"testing"
	"time"
)

// guard-02c state transitions: open -> re-analyzed -> verified improved
// (resolved) or no change (kept open, handed to the plan-stability review)
// -> no second re-ANALYZE. No concurrent access tests: judgeReanalyze and
// followUpFinding are pure.

func openFlipRow(action bool) openFlip {
	o := openFlip{ID: 7, Ident: "queryid:42", Status: "resolved", Severity: "warning",
		Title: "Plan flip after ANALYZE of public.orders for queryid 42",
		Detail: map[string]any{"queryid": float64(42), "relation": "public.orders",
			"previous_plan_hash": "v2:aaaa", "current_plan_hash": "v2:bbbb",
			"blocks_per_call_after": float64(4800)}}
	if action {
		o.ActionID, o.ActionAt = 99, flipT0.Add(4*time.Hour)
	} else {
		o.Status = "open"
	}
	return o
}

func capture(hash string, d time.Duration) *latestCapture {
	return &latestCapture{Hash: hash, At: flipT0.Add(d)}
}

func TestPlanFlip_JudgeReanalyze(t *testing.T) {
	after := 5 * time.Hour
	cases := []struct {
		name   string
		row    openFlip
		latest *latestCapture
		blocks *float64
		want   string
	}{
		{"no action, still flipped", openFlipRow(false), capture("v2:bbbb", after), nil,
			verdictOpen},
		{"no action, no capture", openFlipRow(false), nil, nil, verdictOpen},
		{"no action, plan moved", openFlipRow(false), capture("v2:aaaa", after), nil,
			verdictCleared},
		{"re-analyzed, no capture", openFlipRow(true), nil, nil, verdictPending},
		{"re-analyzed, capture before it", openFlipRow(true), capture("v2:aaaa", 3*time.Hour),
			nil, verdictPending},
		{"re-analyzed, capture at its time", openFlipRow(true), capture("v2:aaaa", 4*time.Hour),
			nil, verdictPending},
		{"re-analyzed, same flipped plan", openFlipRow(true), capture("v2:bbbb", after), nil,
			verdictNoChange},
		{"re-analyzed, earlier plan back", openFlipRow(true), capture("v2:aaaa", after),
			fptr(10), verdictImproved},
		{"re-analyzed, new plan, unknown blocks", openFlipRow(true), capture("v2:cccc", after),
			nil, verdictImproved},
		{"re-analyzed, new plan, more blocks", openFlipRow(true), capture("v2:cccc", after),
			fptr(9000), verdictNoChange},
		{"re-analyzed, new plan, equal blocks", openFlipRow(true), capture("v2:cccc", after),
			fptr(4800), verdictImproved},
		{"re-analyzed, other hash version", openFlipRow(true), capture("v2u:aaaa", after),
			nil, verdictPending},
		{"re-analyzed, empty capture hash", openFlipRow(true), capture("", after), nil,
			verdictPending},
	}
	for _, tc := range cases {
		if got := judgeReanalyze(tc.row, tc.latest, tc.blocks); got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A finding already handed on (no change) closes when the plan moves,
// without being credited to the re-ANALYZE.
func TestPlanFlip_HandedOnFindingClearsWhenThePlanMoves(t *testing.T) {
	row := openFlipRow(true)
	row.Status = "open"
	row.Detail[detailReanalyzeVerdict] = verdictNoChange
	if got := judgeReanalyze(row, capture("v2:aaaa", 6*time.Hour), nil); got != verdictCleared {
		t.Fatalf("verdict = %q, want cleared", got)
	}
	if got := judgeReanalyze(row, capture("v2:bbbb", 6*time.Hour), nil); got != verdictNoChange {
		t.Fatalf("verdict = %q, want no_change kept", got)
	}
}

func TestPlanFlip_JudgeWithoutAFlippedHashKeepsItOpen(t *testing.T) {
	row := openFlipRow(true)
	row.Detail = map[string]any{}
	if got := judgeReanalyze(row, capture("v2:aaaa", 6*time.Hour), nil); got != verdictOpen {
		t.Fatalf("verdict = %q, want open (nothing to compare)", got)
	}
	row.Detail = nil
	if got := judgeReanalyze(row, nil, nil); got != verdictOpen {
		t.Fatalf("nil detail verdict = %q, want open", got)
	}
}

func TestPlanFlip_NoChangeIsHandedOnWithoutASecondAnalyze(t *testing.T) {
	row := openFlipRow(true)
	f := followUpFinding(row, verdictNoChange, capture("v2:bbbb", 5*time.Hour), fptr(5000))
	if f.Category != categoryPlanFlip || f.ObjectIdentifier != row.Ident ||
		f.Severity != "warning" || f.Title != row.Title || f.ObjectType != "query" {
		t.Fatalf("finding = %+v", f)
	}
	if f.RecommendedSQL != "" || f.ActionRisk != "" {
		t.Fatalf("a second ANALYZE was proposed: %q", f.RecommendedSQL)
	}
	d := f.Detail
	if d[detailReanalyzeVerdict] != verdictNoChange || d["handed_to"] != handedToPlanStability ||
		d["reanalyze_action_id"] != int64(99) || d["relation"] != "public.orders" ||
		d["after_plan_hash"] != "v2:bbbb" {
		t.Fatalf("detail = %v", d)
	}
	if b, ok := d["blocks_per_call_after_reanalyze"].(float64); !ok || b != 5000 {
		t.Fatalf("blocks after the re-analyze = %v", d["blocks_per_call_after_reanalyze"])
	}
	if !strings.Contains(f.Recommendation, "statistics target") ||
		!strings.Contains(f.Recommendation, "public.orders") {
		t.Fatalf("recommendation = %q", f.Recommendation)
	}
	if row.Detail[detailReanalyzeVerdict] != nil {
		t.Fatal("the stored row's detail map was written")
	}
}

func TestPlanFlip_FollowUpKeepsAndRecords(t *testing.T) {
	stillOpen := openFlipRow(false)
	stillOpen.Ident = "queryid:43"
	stillOpen.Detail["queryid"] = float64(43)
	db := &fakeFlipDB{now: flipT0.Add(6 * time.Hour),
		open: []openFlip{openFlipRow(true), stillOpen},
		latest: map[int64]*latestCapture{42: capture("v2:aaaa", 5*time.Hour),
			43: capture("v2:bbbb", 5*time.Hour)}}
	a := flipAnalyzer(db)
	if got := a.checkPlanFlips(context.Background(), nil, true); len(got) != 0 {
		t.Fatalf("findings = %+v, want none (improved resolves, open is kept)", got)
	}
	rec := db.recorded[7]
	if rec == nil || rec[detailReanalyzeVerdict] != verdictImproved ||
		rec["after_plan_hash"] != "v2:aaaa" {
		t.Fatalf("recorded verdict = %v, want improved", rec)
	}
	if !a.eval.kept(categoryPlanFlip)["queryid:43"] || a.eval.kept(categoryPlanFlip)["queryid:42"] {
		t.Fatalf("kept = %v, want only the still-flipped query", a.eval.kept(categoryPlanFlip))
	}
}

func TestPlanFlip_FollowUpNoChangeWinsOverADetection(t *testing.T) {
	db := &fakeFlipDB{rels: ordersStats(nil, flipAt(time.Hour)), now: flipT0.Add(6 * time.Hour),
		open:   []openFlip{openFlipRow(true)},
		latest: map[int64]*latestCapture{42: capture("v2:bbbb", 5*time.Hour)}}
	a := flipAnalyzer(db)
	got := a.checkPlanFlips(context.Background(), flipPairs(t), true)
	if len(got) != 1 || got[0].Detail[detailReanalyzeVerdict] != verdictNoChange ||
		got[0].RecommendedSQL != "" {
		t.Fatalf("findings = %+v, want the one handed-on finding", got)
	}
}

// Two queries flipped by the same ANALYZE: one re-ANALYZE covers both, so
// only the first flip of a relation in a cycle carries the SQL.
func TestPlanFlip_OneReanalyzePerRelationPerCycle(t *testing.T) {
	db := &fakeFlipDB{rels: ordersStats(nil, flipAt(time.Hour)), now: flipT0.Add(3 * time.Hour)}
	a := flipAnalyzer(db)
	pairs := flipPairs(t)
	second := pairs[0]
	second.QueryID = 43
	got := a.checkPlanFlips(context.Background(), append(pairs, second), true)
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want two flips", got)
	}
	withSQL := 0
	for _, f := range got {
		if f.RecommendedSQL != "" {
			withSQL++
			continue
		}
		if f.Detail["reanalyze_covered_by"] != "queryid:42" || f.ActionRisk != "" {
			t.Fatalf("held flip = %+v, want it covered by queryid:42", f)
		}
	}
	if withSQL != 1 {
		t.Fatalf("%d flips carry the ANALYZE, want 1", withSQL)
	}
}
