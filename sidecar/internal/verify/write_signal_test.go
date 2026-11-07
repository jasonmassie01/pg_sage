package verify

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// executor-03 phase 1: the database-wide write signal (pg_stat_database
// blk_write_time) is advisory evidence, never a revert reason.
// No concurrent-access tests here: decide and writeSignal are pure functions
// of one observation; concurrent Watch and resume are covered in claim_test.

func writeSignalOf(t *testing.T, verdict Verdict) map[string]any {
	t.Helper()
	signal, ok := verdict.Evidence["write_signal"].(map[string]any)
	if !ok {
		t.Fatalf("evidence = %#v, want a write_signal map", verdict.Evidence)
	}
	if signal["scope"] != "database" || signal["metric"] != "blk_write_time" {
		t.Fatalf("write_signal = %#v, want scope database, metric blk_write_time", signal)
	}
	if signal["decisive"] != false {
		t.Fatalf("write_signal = %#v, want decisive=false", signal)
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

// Queries gain 30% (100 -> 70 ms) while database write time rises 50%
// (10 -> 15 ms), the daytime surge after a night build: the index is kept.
func TestWatchRetainsQueryGainDespiteDatabaseWriteSurge(t *testing.T) {
	source := newFakeObservationSource()
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 15 * time.Millisecond}
	verdict := watchWithWrites(t, source, "write-surge")
	if !verdict.Retain || verdict.Revert || verdict.Outcome != OutcomeImproved {
		t.Fatalf("verdict = %#v, want retained improvement", verdict)
	}
	signal := writeSignalOf(t, verdict)
	if signal["exceeds_threshold"] != true {
		t.Fatalf("write_signal = %#v, want exceeds_threshold=true", signal)
	}
	assertSignalFloat(t, signal, "before_ms", 10)
	assertSignalFloat(t, signal, "after_ms", 15)
	assertSignalFloat(t, signal, "delta_pct", 50)
	assertSignalFloat(t, signal, "threshold_pct", 20)
	if _, hasErr := signal["error"]; hasErr {
		t.Fatalf("write_signal = %#v, want no error", signal)
	}
}

// track_io_timing off: blk_write_time stays 0, so there is no baseline and
// no measured signal (no delta, never over the threshold) and no revert.
func TestWriteSignalWithoutBaselineIsUnavailable(t *testing.T) {
	source := newFakeObservationSource()
	source.writeBefore = Measurement{Samples: 60}
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 40 * time.Millisecond}
	verdict := watchWithWrites(t, source, "write-no-baseline")
	if !verdict.Retain || verdict.Revert {
		t.Fatalf("verdict = %#v, want retained", verdict)
	}
	signal := writeSignalOf(t, verdict)
	if _, has := signal["delta_pct"]; has || signal["exceeds_threshold"] != false {
		t.Fatalf("write_signal = %#v, want no delta and exceeds_threshold=false", signal)
	}
	if reason, _ := signal["unavailable"].(string); !strings.Contains(reason, "baseline") {
		t.Fatalf("write_signal = %#v, want an unavailable reason naming the baseline", signal)
	}
}

// A write-history read error is recorded in the evidence; the verdict is
// still decided by the queries, in both directions.
func TestWriteMeasurementErrorLeavesVerdictToQueries(t *testing.T) {
	cases := map[string]struct {
		after      time.Duration
		wantRetain bool
		wantReason string
	}{
		"query gain is retained":      {70 * time.Millisecond, true, ""},
		"query regression reverts":    {300 * time.Millisecond, false, "query_regression"},
		"neutral query is no_gain":    {100 * time.Millisecond, false, "no_gain"},
		"small gain is still no_gain": {95 * time.Millisecond, false, "no_gain"},
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
				!strings.Contains(msg, "write measurements") {
				t.Fatalf("write_signal = %#v, want the read error with context", signal)
			}
			if signal["exceeds_threshold"] != false {
				t.Fatalf("write_signal = %#v, want exceeds_threshold=false on error", signal)
			}
		})
	}
}

// A rise of exactly write_impact_pct does not exceed it; one nanosecond more
// does. Either way the index is retained.
func TestWriteSignalThresholdBoundary(t *testing.T) {
	cases := map[string]struct {
		after   time.Duration
		exceeds bool
	}{
		"exactly 20%":     {12 * time.Millisecond, false},
		"just above 20%":  {12*time.Millisecond + time.Nanosecond, true},
		"write time fell": {5 * time.Millisecond, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.writeAfter = Measurement{Samples: 60, AverageLatency: tc.after}
			verdict := watchWithWrites(t, source, "write-boundary-"+name)
			if !verdict.Retain || verdict.Revert {
				t.Fatalf("verdict = %#v, want retained", verdict)
			}
			signal := writeSignalOf(t, verdict)
			if signal["exceeds_threshold"] != tc.exceeds {
				t.Fatalf("write_signal = %#v, want exceeds_threshold=%v", signal, tc.exceeds)
			}
		})
	}
}

// A neutral query result with a write surge used to revert as a write
// regression (that case came first); it is now no_gain, its own reason.
func TestNeutralQueriesWithWriteSurgeRevertAsNoGain(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 60, AverageLatency: 100 * time.Millisecond,
		Buckets: 12}
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 30 * time.Millisecond}
	verdict := watchWithWrites(t, source, "neutral-surge")
	if !verdict.Revert || verdict.Reason != "no_gain" || verdict.Outcome != OutcomeNeutral {
		t.Fatalf("verdict = %#v, want a no_gain revert with a neutral outcome", verdict)
	}
	if writeSignalOf(t, verdict)["exceeds_threshold"] != true {
		t.Fatalf("write_signal must still report the surge")
	}
}

// Every decided or extended verdict carries the signal, and no revert
// reason outside the query, validity and sample reasons is ever produced.
func TestRevertReasonsExcludeTheWriteSignal(t *testing.T) {
	allowed := map[string]bool{"query_regression": true, "no_gain": true,
		"invalid_index": true, "insufficient_samples": true}
	afters := []Measurement{
		{Samples: 60, AverageLatency: 70 * time.Millisecond, Buckets: 12},
		{Samples: 60, AverageLatency: 300 * time.Millisecond, Buckets: 12},
		{Samples: 60, AverageLatency: 100 * time.Millisecond, Buckets: 12},
		{Samples: 5, AverageLatency: 70 * time.Millisecond, Buckets: 12},
	}
	for i, after := range afters {
		source := newFakeObservationSource()
		source.after[42] = after
		source.writeAfter = Measurement{Samples: 60, AverageLatency: time.Second}
		verdict := watchWithWrites(t, source, "reasons-"+string(rune('a'+i)))
		if verdict.Revert && !allowed[verdict.Reason] {
			t.Fatalf("case %d: revert reason %q is not a query/validity/sample reason",
				i, verdict.Reason)
		}
		if verdict.Retain && verdict.Revert {
			t.Fatalf("case %d: verdict both retains and reverts: %#v", i, verdict)
		}
		writeSignalOf(t, verdict)
	}
}

func TestWriteSignalStateTransitionsOnResume(t *testing.T) {
	// An extended (insufficient-samples) watch carries the signal, and the
	// later resumed decision is still made by the queries alone.
	store := newMemoryStateStore()
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 10, AverageLatency: 70 * time.Millisecond,
		Buckets: 12}
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 50 * time.Millisecond}
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
	if !got.Retain || got.Revert || writeSignalOf(t, got)["exceeds_threshold"] != true {
		t.Fatalf("resumed verdict = %#v, want retained with the surge recorded", got)
	}
}

func TestWriteSignalPureFunctionEdges(t *testing.T) {
	// Zero-value observation and criterion: no baseline, not decisive.
	signal := writeSignal(Criterion{}, observations{})
	if signal["decisive"] != false || signal["exceeds_threshold"] != false {
		t.Fatalf("zero signal = %#v", signal)
	}
	if _, has := signal["delta_pct"]; has {
		t.Fatalf("zero signal = %#v, want no delta", signal)
	}
	// A negative baseline (a counter reset clamps to 0 in SQL, but never
	// trust it) is treated as no baseline, not as a huge rise.
	signal = writeSignal(Criterion{WriteImpactPct: 20}, observations{
		writeBefore: Measurement{AverageLatency: -time.Millisecond},
		writeAfter:  Measurement{AverageLatency: time.Millisecond},
	})
	if signal["exceeds_threshold"] != false || signal["unavailable"] == nil {
		t.Fatalf("negative baseline signal = %#v, want unavailable", signal)
	}
}
