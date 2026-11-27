package analyzer

import (
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-10: seq_scan_heavy judges the scans of the interval between two
// snapshots of the same relation stats epoch, not lifetime counters: an
// old seq-scan storm stops being flagged and a new storm on an
// index-heavy table becomes visible.

// No concurrent access tests: ruleSeqScanWatchdog is a pure function of
// the two snapshots; runSeqScanWatchdog runs once per analyzer cycle.

var seqEpoch = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

type scans struct{ seq, idx int64 }

// seqPair builds two snapshots 60 seconds apart in one stats epoch; each
// table is named, with its previous and current counters.
func seqPair(tables map[string][2]scans) (prev, cur *collector.Snapshot) {
	at := seqEpoch.Add(time.Hour)
	prev = &collector.Snapshot{CollectedAt: at,
		System: collector.SystemStats{RelationStatsEpoch: seqEpoch}}
	cur = &collector.Snapshot{CollectedAt: at.Add(60 * time.Second),
		System: collector.SystemStats{RelationStatsEpoch: seqEpoch}}
	for name, c := range tables {
		row := collector.TableStats{SchemaName: "public", RelName: name, NLiveTup: 100000}
		p, n := row, row
		p.SeqScan, p.IdxScan = c[0].seq, c[0].idx
		n.SeqScan, n.IdxScan = c[1].seq, c[1].idx
		prev.Tables = append(prev.Tables, p)
		cur.Tables = append(cur.Tables, n)
	}
	return prev, cur
}

func seqIdents(findings []Finding) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range findings {
		if f.Category == "seq_scan_heavy" {
			out[f.ObjectIdentifier] = f
		}
	}
	return out
}

func TestSeqScanDelta_IntervalStormFlagged(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{"orders": {{1000, 50}, {1101, 50}}})
	got := seqIdents(ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil))
	f, ok := got["public.orders"]
	if !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.orders", got)
	}
	want := map[string]any{"seq_scan": int64(1101), "idx_scan": int64(50),
		"interval_seq_scan": int64(101), "interval_idx_scan": int64(0),
		"interval_seconds": float64(60), "n_live_tup": int64(100000)}
	for k, v := range want {
		if f.Detail[k] != v {
			t.Errorf("detail[%s] = %v (%T), want %v (%T)", k, f.Detail[k], f.Detail[k], v, v)
		}
	}
	if f.Severity != "warning" || f.ActionRisk != "safe" {
		t.Errorf("severity/risk = %s/%s", f.Severity, f.ActionRisk)
	}
}

// An old storm: lifetime seq scans dwarf index scans, but nothing scanned
// in the interval. Today's lifetime rule flags it forever.
func TestSeqScanDelta_OldStormNotFlagged(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{"archive": {{1000000, 0}, {1000000, 0}}})
	if got := ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil); len(got) != 0 {
		t.Fatalf("idle old storm flagged: %+v", got)
	}
}

// A new storm on an index-heavy table: lifetime index scans hide it from
// today's rule.
func TestSeqScanDelta_NewStormOnIndexHeavyTableFlagged(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{
		"users": {{5000, 1000000}, {5500, 1000010}}})
	got := seqIdents(ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil))
	f, ok := got["public.users"]
	if !ok {
		t.Fatalf("new storm on an index-heavy table not flagged: %v", got)
	}
	if f.Detail["interval_seq_scan"] != int64(500) || f.Detail["interval_idx_scan"] != int64(10) {
		t.Errorf("detail = %v", f.Detail)
	}
}

func TestSeqScanDelta_Boundaries(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{
		"d100":        {{0, 0}, {100, 0}},   // Δseq 100: not > 100
		"d101_x10":    {{0, 0}, {101, 10}},  // Δseq 101 > 10 x 10
		"d101_x11":    {{0, 0}, {101, 11}},  // Δseq 101 <= 10 x 11
		"d200_equal":  {{0, 0}, {200, 20}},  // Δseq == 10 x Δidx
		"d201_over20": {{0, 0}, {201, 20}},  // Δseq > 10 x Δidx
		"d101_noidx":  {{50, 0}, {151, 0}},  // Δseq 101, no index scans
		"d100_noidx":  {{50, 0}, {150, 0}},  // Δseq 100, no index scans
		"d0":          {{900, 0}, {900, 0}}, // idle
	})
	got := seqIdents(ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil))
	for name, want := range map[string]bool{"d100": false, "d101_x10": true,
		"d101_x11": false, "d200_equal": false, "d201_over20": true,
		"d101_noidx": true, "d100_noidx": false, "d0": false} {
		if _, flagged := got["public."+name]; flagged != want {
			t.Errorf("%s flagged = %v, want %v", name, flagged, want)
		}
	}
}

// Nil/empty: with no previous snapshot the rule has no interval and
// emits nothing.
func TestSeqScanDelta_NilPreviousNoFindings(t *testing.T) {
	_, cur := seqPair(map[string][2]scans{"orders": {{0, 0}, {5000, 0}}})
	if got := ruleSeqScanWatchdog(cur, nil, coverageTestConfig(), nil); len(got) != 0 {
		t.Fatalf("nil previous gave findings: %+v", got)
	}
}

// Invalid input: a moved stats epoch (pg_stat_reset, restart) or an
// unknown one gives no interval and so no finding.
func TestSeqScanDelta_EpochMovedOrUnknownNoFindings(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{"orders": {{0, 0}, {5000, 0}}})
	cur.System.RelationStatsEpoch = seqEpoch.Add(30 * time.Minute)
	if got := ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil); len(got) != 0 {
		t.Errorf("moved epoch gave findings: %+v", got)
	}
	prev.System.RelationStatsEpoch, cur.System.RelationStatsEpoch = time.Time{}, time.Time{}
	if got := ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil); len(got) != 0 {
		t.Errorf("unknown epoch gave findings: %+v", got)
	}
}

// Invalid input: a counter that went down (a single-table reset the epoch
// did not show) gives no finding and never a negative delta; the other
// tables are judged normally.
func TestSeqScanDelta_DecreasedCounterSkipped(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{
		"reset_seq": {{9000, 0}, {200, 0}},
		"reset_idx": {{100, 9000}, {400, 5}},
		"storm":     {{0, 0}, {500, 0}},
	})
	got := seqIdents(ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil))
	if _, ok := got["public.storm"]; !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.storm only", got)
	}
	for _, f := range got {
		for _, k := range []string{"interval_seq_scan", "interval_idx_scan"} {
			if v, _ := f.Detail[k].(int64); v < 0 {
				t.Errorf("%s %s = %d", f.ObjectIdentifier, k, v)
			}
		}
	}
	undecided := seqScanUndecided(cur, prev)
	if len(undecided) != 2 || !slices.Contains(undecided, "public.reset_seq") ||
		!slices.Contains(undecided, "public.reset_idx") {
		t.Errorf("undecided = %v, want both reset tables", undecided)
	}
}

// A table absent from the previous snapshot has no interval yet: no
// finding, and its open finding (if any) stays open.
func TestSeqScanDelta_NewTableUndecided(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{"old": {{0, 0}, {0, 0}}})
	cur.Tables = append(cur.Tables, collector.TableStats{SchemaName: "public",
		RelName: "brand_new", SeqScan: 9000, NLiveTup: 100000})
	if got := ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), nil); len(got) != 0 {
		t.Errorf("table without a previous row flagged: %+v", got)
	}
	if u := seqScanUndecided(cur, prev); len(u) != 1 || u[0] != "public.brand_new" {
		t.Errorf("undecided = %v, want public.brand_new", u)
	}
}

func TestSeqScanDelta_SmallTableSkipAndUnlogged(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{
		"small": {{0, 0}, {5000, 0}}, "fk_table": {{0, 0}, {5000, 0}},
		"cache_tbl": {{0, 0}, {5000, 0}}})
	for i := range cur.Tables {
		switch cur.Tables[i].RelName {
		case "small":
			cur.Tables[i].NLiveTup = 9999
		case "cache_tbl":
			cur.Tables[i].Relpersistence = "u"
		}
	}
	skip := map[string]bool{"public.fk_table": true}
	got := seqIdents(ruleSeqScanWatchdog(cur, prev, coverageTestConfig(), skip))
	f, ok := got["public.cache_tbl"]
	if !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.cache_tbl only", got)
	}
	if f.Detail["unlogged"] != true {
		t.Errorf("unlogged detail missing: %v", f.Detail)
	}
}

// Readiness: the watchdog is evaluated only with a usable interval; with
// none, seq_scan_heavy is marked failed so its open findings stay open.
func TestRunSeqScanWatchdog_EvaluatedOnlyWithInterval(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{"orders": {{0, 0}, {50, 0}}})
	moved := *cur
	moved.System.RelationStatsEpoch = seqEpoch.Add(time.Minute)
	noTables := *prev
	noTables.Unavailable = map[string]string{"tables": "timeout"}
	for name, c := range map[string]struct {
		prev, cur *collector.Snapshot
		ready     bool
	}{
		"nil previous":   {nil, cur, false},
		"moved epoch":    {prev, &moved, false},
		"prev no tables": {&noTables, cur, false},
		"interval":       {prev, cur, true},
	} {
		a := &Analyzer{cfg: coverageTestConfig(), eval: newCycleEval(),
			logFn: func(string, string, ...any) {}}
		a.runSeqScanWatchdog(c.cur, c.prev, nil)
		failed := a.eval.hasFailed("seq_scan_heavy")
		resolvable := a.eval.resolvable(nil)["seq_scan_heavy"]
		if failed == c.ready || resolvable != c.ready {
			t.Errorf("%s: failed=%v resolvable=%v, want evaluated=%v", name,
				failed, resolvable, c.ready)
		}
	}
}

// State transition: a table flagged in one interval resolves in the next
// idle interval, and the reset table's identity is kept open by the
// cycle.
func TestRunSeqScanWatchdog_KeepsUndecidedOpen(t *testing.T) {
	prev, cur := seqPair(map[string][2]scans{
		"storm": {{0, 0}, {500, 0}}, "reset": {{9000, 0}, {10, 0}}})
	a := &Analyzer{cfg: coverageTestConfig(), eval: newCycleEval(),
		logFn: func(string, string, ...any) {}}
	got := seqIdents(a.runSeqScanWatchdog(cur, prev, nil))
	if _, ok := got["public.storm"]; !ok || len(got) != 1 {
		t.Fatalf("first interval findings = %v", got)
	}
	if !a.eval.kept("seq_scan_heavy")["public.reset"] {
		t.Errorf("reset table not kept open: %v", a.eval.kept("seq_scan_heavy"))
	}
	next := *cur
	next.CollectedAt = cur.CollectedAt.Add(time.Minute)
	if again := a.runSeqScanWatchdog(&next, cur, nil); len(again) != 0 {
		t.Errorf("idle next interval still flags: %+v", again)
	}
}

// zeroPrevious returns a previous snapshot of cur's tables with every scan
// counter at zero, in the same stats epoch: the interval is then the
// lifetime counters, as the pre-rules-10 fixtures below assumed.
func zeroPrevious(cur *collector.Snapshot) *collector.Snapshot {
	cur.System.RelationStatsEpoch = seqEpoch
	if cur.CollectedAt.IsZero() {
		cur.CollectedAt = seqEpoch.Add(time.Hour)
	}
	prev := &collector.Snapshot{CollectedAt: cur.CollectedAt.Add(-time.Minute),
		System: collector.SystemStats{RelationStatsEpoch: seqEpoch}}
	for _, t := range cur.Tables {
		t.SeqScan, t.IdxScan = 0, 0
		prev.Tables = append(prev.Tables, t)
	}
	return prev
}
