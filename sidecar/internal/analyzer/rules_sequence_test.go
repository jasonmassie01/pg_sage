package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// No concurrent access tests: ruleSequenceExhaustion is a pure function of
// the snapshot it is given and holds no shared state.

func seqSnap(seqs ...collector.SequenceStats) *collector.Snapshot {
	return &collector.Snapshot{Sequences: seqs}
}

func ascSeq(name string, pct float64) collector.SequenceStats {
	return collector.SequenceStats{
		SchemaName: "public", SequenceName: name, DataType: "integer",
		LastValue: 1, MinValue: 1, MaxValue: 2_147_483_647, IncrementBy: 1,
		PctUsed: pct,
	}
}

func onlySequenceFinding(t *testing.T, ff []Finding) Finding {
	t.Helper()
	if len(ff) != 1 {
		t.Fatalf("findings = %d (%+v), want 1", len(ff), ff)
	}
	if ff[0].Category != "sequence_exhaustion" || ff[0].ObjectType != "sequence" {
		t.Fatalf("finding = %+v, want a sequence_exhaustion finding on a sequence", ff[0])
	}
	return ff[0]
}

// A CYCLE sequence wraps by design: reaching the end of its range is its
// contract, not an outage, so it is never reported (the runway skips it too).
func TestSequenceExhaustion_CycleSequenceSkipped(t *testing.T) {
	for _, pct := range []float64{75, 95, 100} {
		seq := ascSeq("wraps", pct)
		seq.Cycle = true
		if ff := ruleSequenceExhaustion(seqSnap(seq), nil, nil, nil); len(ff) != 0 {
			t.Fatalf("cycle sequence at %.1f%%: findings %+v, want none", pct, ff)
		}
	}
}

// The cycle skip is per sequence: a non-cycle sequence beside a cycle one
// is still reported.
func TestSequenceExhaustion_CycleSkipIsPerSequence(t *testing.T) {
	wraps := ascSeq("wraps", 99)
	wraps.Cycle = true
	f := onlySequenceFinding(t, ruleSequenceExhaustion(
		seqSnap(wraps, ascSeq("ends", 99)), nil, nil, nil))
	if f.ObjectIdentifier != "public.ends" || f.Severity != "critical" {
		t.Fatalf("finding = %s %s, want public.ends critical", f.ObjectIdentifier, f.Severity)
	}
}

// The 75% and 90% thresholds are inclusive lower bounds.
func TestSequenceExhaustion_ThresholdBoundaries(t *testing.T) {
	cases := []struct {
		pct  float64
		want string // "" means no finding
	}{
		{0, ""}, {74.99, ""}, {75.0, "warning"}, {89.99, "warning"},
		{90.0, "critical"}, {100, "critical"},
	}
	for _, c := range cases {
		ff := ruleSequenceExhaustion(seqSnap(ascSeq("s", c.pct)), nil, nil, nil)
		if c.want == "" {
			if len(ff) != 0 {
				t.Errorf("%.2f%%: findings %+v, want none", c.pct, ff)
			}
			continue
		}
		if len(ff) != 1 || ff[0].Severity != c.want {
			t.Errorf("%.2f%%: findings %+v, want one %s", c.pct, ff, c.want)
			continue
		}
		if got := ff[0].Detail["usage_pct"]; got != c.pct {
			t.Errorf("%.2f%%: usage_pct = %v, want the collector's %.2f", c.pct, got, c.pct)
		}
	}
}

// A descending sequence with a custom MINVALUE is judged by the collector's
// pct_used over [min_value, max_value] in the direction of travel, not by
// the share of the data type's minimum (which is about 0% here).
func TestSequenceExhaustion_DescendingCustomMinValue(t *testing.T) {
	down := collector.SequenceStats{
		SchemaName: "public", SequenceName: "down", DataType: "bigint",
		LastValue: -950, MinValue: -1000, MaxValue: -1, IncrementBy: -1,
		PctUsed: 94.99, // (max - last) / (max - min) = 949 / 999
	}
	f := onlySequenceFinding(t, ruleSequenceExhaustion(seqSnap(down), nil, nil, nil))
	if f.Severity != "critical" {
		t.Fatalf("severity = %s, want critical at 94.99%%", f.Severity)
	}
	if got := f.Detail["usage_pct"]; got != 94.99 {
		t.Fatalf("usage_pct = %v, want the collector's 94.99", got)
	}
	if got := f.Detail["min_value"]; got != int64(-1000) {
		t.Fatalf("min_value = %v, want -1000", got)
	}
	if !strings.Contains(f.Title, "95.0%") {
		t.Fatalf("title = %q, want the collector's share (95.0%%)", f.Title)
	}
}

// A descending sequence far from its custom minimum is not reported, even
// when the data type's range would say otherwise.
func TestSequenceExhaustion_DescendingLowUsageNotReported(t *testing.T) {
	down := collector.SequenceStats{
		SchemaName: "public", SequenceName: "down", DataType: "integer",
		LastValue: -10, MinValue: -1000, MaxValue: -1, IncrementBy: -1,
		PctUsed: 0.9,
	}
	if ff := ruleSequenceExhaustion(seqSnap(down), nil, nil, nil); len(ff) != 0 {
		t.Fatalf("findings %+v, want none at 0.9%%", ff)
	}
}

// Happy path: an ascending int4 sequence at 91% is critical, keeps the
// bigint advice and points at the runway finding for time-to-limit.
func TestSequenceExhaustion_AscendingCriticalCrossLinksRunway(t *testing.T) {
	seq := ascSeq("orders_id_seq", 91)
	seq.LastValue = 1_954_210_118
	f := onlySequenceFinding(t, ruleSequenceExhaustion(seqSnap(seq), nil, nil, nil))
	if f.Severity != "critical" || f.ObjectIdentifier != "public.orders_id_seq" ||
		f.ActionRisk != "safe" {
		t.Fatalf("finding = %+v, want a safe critical on public.orders_id_seq", f)
	}
	want := map[string]any{
		"current_value": int64(1_954_210_118), "max_value": int64(2_147_483_647),
		"min_value": int64(1), "increment": int64(1), "usage_pct": 91.0,
		"data_type": "integer", "cycle": false,
		"runway_category": "forecast_sequence_runway",
	}
	for k, v := range want {
		if f.Detail[k] != v {
			t.Errorf("detail[%s] = %#v, want %#v", k, f.Detail[k], v)
		}
	}
	for _, s := range []string{"bigint", "forecast_sequence_runway", "time to the limit",
		"owning column"} {
		if !strings.Contains(f.Recommendation, s) {
			t.Errorf("recommendation %q lacks %q", f.Recommendation, s)
		}
	}
}

// A bigint sequence gets the range advice, and the runway cross-link too.
func TestSequenceExhaustion_BigintRecommendation(t *testing.T) {
	seq := ascSeq("big", 80)
	seq.DataType = "bigint"
	f := onlySequenceFinding(t, ruleSequenceExhaustion(seqSnap(seq), nil, nil, nil))
	if strings.Contains(f.Recommendation, "migrating to bigint") ||
		!strings.Contains(f.Recommendation, "forecast_sequence_runway") {
		t.Fatalf("recommendation = %q, want range advice and the runway link", f.Recommendation)
	}
}

// An unreadable sequence has no last value: the collector reports 0, and
// the rule reports nothing, in either direction.
func TestSequenceExhaustion_UnreadableSequenceGivesNone(t *testing.T) {
	up := ascSeq("hidden_up", 0)
	up.LastValue = 0
	down := collector.SequenceStats{
		SchemaName: "public", SequenceName: "hidden_down", DataType: "integer",
		MinValue: -2_147_483_648, MaxValue: -1, IncrementBy: -1,
	}
	if ff := ruleSequenceExhaustion(seqSnap(up, down), nil, nil, nil); len(ff) != 0 {
		t.Fatalf("findings %+v, want none for unreadable sequences", ff)
	}
}

// No sequences: the hasSequences gate keeps the rule from being evaluated,
// so it cannot resolve open findings on input it never saw; called anyway,
// it reports nothing.
func TestSequenceExhaustion_NoSequencesNotEvaluated(t *testing.T) {
	var spec *RuleSpec
	for i := range AllRules {
		if AllRules[i].Name == "sequence_exhaustion" {
			spec = &AllRules[i]
		}
	}
	if spec == nil || spec.Needs == nil {
		t.Fatalf("sequence_exhaustion spec = %+v, want a registered rule with a gate", spec)
	}
	for _, snap := range []*collector.Snapshot{{}, {Sequences: []collector.SequenceStats{}}} {
		if spec.Needs(snap, nil) {
			t.Fatalf("gate open for %+v, want closed with no sequences", snap.Sequences)
		}
		if ff := ruleSequenceExhaustion(snap, nil, nil, nil); len(ff) != 0 {
			t.Fatalf("findings %+v, want none", ff)
		}
	}
	if !spec.Needs(seqSnap(ascSeq("s", 1)), nil) {
		t.Fatal("gate closed with one sequence, want open")
	}
}
