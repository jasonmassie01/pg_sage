package verify

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// executor-03b: the write signal is table-scoped (WAL bytes per tuple the
// index's table wrote) and reverts as write_impact above
// verify.write_impact_pct, only for an index whose queries improved: the
// query regression and no_gain reasons keep their precedence. Without
// attributable table samples it is insufficient, never a revert and never a
// database-wide fallback.
// No concurrent-access tests here: decide and writeSignal are pure functions
// of one observation; concurrent Watch and resume are covered in claim_test.

func writeSignalOf(t *testing.T, verdict Verdict) map[string]any {
	t.Helper()
	signal, ok := verdict.Evidence["write_signal"].(map[string]any)
	if !ok {
		t.Fatalf("evidence = %#v, want a write_signal map", verdict.Evidence)
	}
	if signal["scope"] != "table" || signal["metric"] != "wal_bytes_per_tuple" {
		t.Fatalf("write_signal = %#v, want scope table, metric wal_bytes_per_tuple", signal)
	}
	return signal
}

func assertSignalFloat(t *testing.T, signal map[string]any, key string, want float64) {
	t.Helper()
	got, ok := signal[key].(float64)
	if !ok || math.Abs(got-want) > 1e-6 {
		t.Fatalf("write_signal[%s] = %#v, want %v", key, signal[key], want)
	}
}

func watchWithWrites(t *testing.T, source *fakeObservationSource, id string) Verdict {
	t.Helper()
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest(id),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	return verdict
}

// Queries gain 30% (100 -> 70 ms) but every tuple written to the table now
// costs 50% more WAL (100 -> 150 bytes): the index reverts as write_impact.
func TestWatchRevertsTableWriteRegression(t *testing.T) {
	source := newFakeObservationSource()
	source.writeAfter = measuredCost(150)
	verdict := watchWithWrites(t, source, "table-write-regression")
	if !verdict.Revert || verdict.Retain || verdict.Status != "reverted" ||
		verdict.Reason != "write_impact" || verdict.Outcome != OutcomeRegressed {
		t.Fatalf("verdict = %#v, want a write_impact revert", verdict)
	}
	if verdict.ObservedPct == nil || math.Abs(*verdict.ObservedPct+30) > 1e-6 {
		t.Fatalf("observed = %v, want the queries' -30%%", verdict.ObservedPct)
	}
	signal := writeSignalOf(t, verdict)
	if signal["outcome"] != writeRegressed || signal["decisive"] != true ||
		signal["exceeds_threshold"] != true || signal["table"] != "public.orders" {
		t.Fatalf("write_signal = %#v, want a decisive regression on public.orders", signal)
	}
	assertSignalFloat(t, signal, "delta_pct", 50)
	assertSignalFloat(t, signal, "threshold_pct", 20)
	before, _ := signal["before"].(map[string]any)
	after, _ := signal["after"].(map[string]any)
	if before["bytes_per_tuple"] != 100.0 || after["bytes_per_tuple"] != 150.0 ||
		after["intervals"] != 6 || after["tuples"] != int64(6000) {
		t.Fatalf("write_signal sides = %#v / %#v, want the measured costs", before, after)
	}
}

// A rise of exactly write_impact_pct does not revert; any more does.
func TestWatchWriteImpactThresholdBoundary(t *testing.T) {
	cases := map[string]struct {
		after  float64
		revert bool
	}{
		"exactly 20%":    {120, false},
		"just above 20%": {120.0001, true},
		"cost fell":      {60, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.writeAfter = measuredCost(tc.after)
			verdict := watchWithWrites(t, source, "write-boundary-"+name)
			if verdict.Revert != tc.revert || verdict.Retain == tc.revert {
				t.Fatalf("verdict = %#v, want revert=%v", verdict, tc.revert)
			}
			signal := writeSignalOf(t, verdict)
			if signal["exceeds_threshold"] != tc.revert || signal["decisive"] != tc.revert {
				t.Fatalf("write_signal = %#v, want exceeds=decisive=%v", signal, tc.revert)
			}
			if !tc.revert && (verdict.Status != "success" || signal["outcome"] !=
				writeWithinThreshold) {
				t.Fatalf("verdict = %#v, want a retained index within the threshold", verdict)
			}
		})
	}
}

// No attributable table samples (no tables snapshots, or only intervals
// in which other tables wrote): insufficient, so the queries decide, and
// no database-wide number stands in for the table.
func TestWatchWithoutTableSamplesIsInsufficient(t *testing.T) {
	cases := map[string]WriteCost{
		"no samples":        {},
		"other tables only": {Unattributed: 12},
		"unknown intervals": {Unknown: 12},
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.writeAfter = after
			verdict := watchWithWrites(t, source, "write-insufficient-"+name)
			if !verdict.Retain || verdict.Revert || verdict.Outcome != OutcomeImproved {
				t.Fatalf("verdict = %#v, want the improving index retained", verdict)
			}
			signal := writeSignalOf(t, verdict)
			if signal["outcome"] != writeInsufficient || signal["decisive"] != false ||
				signal["exceeds_threshold"] != false {
				t.Fatalf("write_signal = %#v, want insufficient and not decisive", signal)
			}
			if _, has := signal["delta_pct"]; has {
				t.Fatalf("write_signal = %#v, want no delta without samples", signal)
			}
			if reason, _ := signal["insufficient"].(string); !strings.Contains(reason,
				"attributable") {
				t.Fatalf("write_signal = %#v, want a reason naming attributable samples",
					signal)
			}
		})
	}
}

// A write-history read error is recorded; the write evidence is then
// insufficient and the queries decide, in every direction, without pausing
// the watch (executor-02's observation retries are for the query evidence).
func TestWriteCostErrorLeavesVerdictToQueries(t *testing.T) {
	cases := map[string]struct {
		after      time.Duration
		wantRetain bool
		wantReason string
	}{
		"query gain is retained":   {70 * time.Millisecond, true, ""},
		"query regression reverts": {300 * time.Millisecond, false, "query_regression"},
		"neutral query is no_gain": {100 * time.Millisecond, false, "no_gain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.after[42] = Measurement{Samples: 60, AverageLatency: tc.after, Buckets: 12}
			source.writeErr = errors.New("snapshots relation is locked")
			verdict := watchWithWrites(t, source, "write-err-"+name)
			if verdict.Retain != tc.wantRetain || verdict.Revert == tc.wantRetain ||
				verdict.Reason != tc.wantReason || verdict.ObservationFailures != 0 {
				t.Fatalf("verdict = %#v, want retain=%v reason=%q decided by queries",
					verdict, tc.wantRetain, tc.wantReason)
			}
			signal := writeSignalOf(t, verdict)
			msg, _ := signal["error"].(string)
			if !strings.Contains(msg, "snapshots relation is locked") ||
				!strings.Contains(msg, "write cost") || signal["outcome"] != writeInsufficient {
				t.Fatalf("write_signal = %#v, want the read error with context", signal)
			}
		})
	}
}

// The query reasons come first: a write regression never turns a query
// regression or a no_gain into write_impact, and never ends an extended
// (insufficient-samples) window early.
func TestQueryReasonsTakePrecedenceOverWriteImpact(t *testing.T) {
	cases := map[string]struct {
		after      Measurement
		wantStatus string
		wantReason string
	}{
		"query regression": {Measurement{Samples: 60, AverageLatency: 300 * time.Millisecond,
			Buckets: 12}, "reverted", "query_regression"},
		"neutral queries": {Measurement{Samples: 60, AverageLatency: 100 * time.Millisecond,
			Buckets: 12}, "reverted", "no_gain"},
		"too few samples": {Measurement{Samples: 5, AverageLatency: 70 * time.Millisecond,
			Buckets: 12}, "extended", "insufficient_samples"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.after[42] = tc.after
			source.writeAfter = measuredCost(400)
			verdict := watchWithWrites(t, source, "precedence-"+name)
			if verdict.Status != tc.wantStatus || verdict.Reason != tc.wantReason {
				t.Fatalf("verdict = %#v, want %s %s", verdict, tc.wantStatus, tc.wantReason)
			}
			signal := writeSignalOf(t, verdict)
			if signal["decisive"] != false || signal["outcome"] != writeRegressed {
				t.Fatalf("write_signal = %#v, want a recorded, non-decisive regression",
					signal)
			}
		})
	}
}

// The signal reads the watched table over the pre-action window
// [executedAt-window, executedAt] and the post-action one [executedAt, now].
func TestWriteSignalReadsTheWatchedTableWindows(t *testing.T) {
	source := newFakeObservationSource()
	watchWithWrites(t, source, "write-windows")
	request := successfulWatchRequest("write-windows")
	want := []writeRead{
		{table: "public.orders", from: request.ExecutedAt.Add(-2 * time.Hour),
			to: request.ExecutedAt},
		{table: "public.orders", from: request.ExecutedAt, to: testVerificationNow()},
	}
	if len(source.writeReads) != 2 || source.writeReads[0] != want[0] ||
		source.writeReads[1] != want[1] {
		t.Fatalf("write reads = %#v, want %#v", source.writeReads, want)
	}
}

// State transition: an extended watch records the signal, and the resumed
// decision reverts on the table's write regression once the queries have
// enough samples; the durable state keeps the write_impact reason.
func TestWriteImpactAfterResume(t *testing.T) {
	store := newMemoryStateStore()
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 10, AverageLatency: 70 * time.Millisecond,
		Buckets: 12}
	source.writeAfter = measuredCost(200)
	first, err := newTestEngine(t, source, store).Watch(context.Background(),
		successfulWatchRequest("resume-signal"))
	if err != nil || first.Status != "extended" {
		t.Fatalf("Watch = %#v, %v; want extended", first, err)
	}
	writeSignalOf(t, first)
	source.after[42] = Measurement{Samples: 60, AverageLatency: 70 * time.Millisecond,
		Buckets: 12}
	options := DefaultOptions()
	options.Now = func() time.Time { return testVerificationNow().Add(3 * time.Hour) }
	later, err := NewEngine(source, store, options)
	if err != nil {
		t.Fatal(err)
	}
	results, err := later.ResumeDueResults(context.Background())
	if err != nil || len(results) != 1 {
		t.Fatalf("ResumeDueResults = %#v, %v", results, err)
	}
	got := results[0].Verdict
	if !got.Revert || got.Reason != "write_impact" || writeSignalOf(t, got)["decisive"] != true {
		t.Fatalf("resumed verdict = %#v, want a decisive write_impact revert", got)
	}
	stored, err := store.Get(context.Background(), "resume-signal")
	if err != nil || stored.Reason != "write_impact" || stored.Status != "reverted" {
		t.Fatalf("stored = %#v, %v; want the write_impact revert persisted", stored, err)
	}
}

func TestWriteSignalPureFunctionEdges(t *testing.T) {
	// Zero-value criterion and observation: insufficient, not decisive.
	signal := writeSignal(WatchState{}, observations{}, false)
	if signal["decisive"] != false || signal["exceeds_threshold"] != false ||
		signal["outcome"] != writeInsufficient {
		t.Fatalf("zero signal = %#v", signal)
	}
	if _, has := signal["delta_pct"]; has {
		t.Fatalf("zero signal = %#v, want no delta", signal)
	}
}
