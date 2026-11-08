package runway

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-02: a series the monitor could not measure (too few samples,
// too short a span, an unknown rate, no samples at all, or rows the
// trends read was cut before) is not a cleared runway. Its open finding
// is held (kept open, marked stale) instead of resolved, and a truncated
// trends read leaves the categories it may have cut unevaluated.
//
// No concurrency test: Evaluate and Holds are pure functions of their
// input and share nothing.

func xidTrend(n int64, span time.Duration, in time.Duration) probes.RunwayTrend {
	return trend(probes.RunwayXID, probes.SubjectCluster, n, span, 1.5e9,
		WraparoundWarnAge, rateFor(1.5e9, WraparoundWarnAge, in), 1)
}

func heldReason(h map[string]map[string]string, cat, ident string) string {
	return h[cat][ident]
}

func hasCategory(cats []string, want string) bool {
	for _, c := range cats {
		if c == want {
			return true
		}
	}
	return false
}

// State transitions of one open critical XID finding: unmeasured it is
// held; measured inside the horizon it is a runway; measured outside it
// is neither, so it may resolve.
func TestHolds_XIDStateTransitions(t *testing.T) {
	opts := testOptions()
	cases := []struct {
		name       string
		tr         probes.RunwayTrend
		wantRunway bool
		wantHeld   string
	}{
		{"three samples", xidTrend(3, time.Hour, 24*time.Hour), false, HoldUnmeasured},
		{"inside the horizon", xidTrend(12, time.Hour, 24*time.Hour), true, ""},
		{"outside the horizon", xidTrend(12, time.Hour, 400*time.Hour), false, ""},
	}
	for _, c := range cases {
		in := EvalInput{Trends: []probes.RunwayTrend{c.tr}, TrendsOK: true, TablesOK: true,
			XID: xidReading(1.5e9, 2e8)} // past the freeze maximum (forecast-01)
		rs, evaluated := Evaluate(in, opts)
		if got := len(rs) == 1 && rs[0].Identifier == "xid"; got != c.wantRunway ||
			len(rs) > 1 {
			t.Errorf("%s: runways %+v, want runway=%v", c.name, rs, c.wantRunway)
		}
		if got := heldReason(Holds(in, opts), CategoryWraparound, "xid"); got != c.wantHeld {
			t.Errorf("%s: held %q, want %q", c.name, got, c.wantHeld)
		}
		if !hasCategory(evaluated, CategoryWraparound) {
			t.Errorf("%s: wraparound not evaluated (%v)", c.name, evaluated)
		}
	}
}

// Boundary: MinSamples samples over exactly MinSpan is measured; one
// sample fewer, one second less or an unknown rate is held.
func TestHolds_MeasuredBoundary(t *testing.T) {
	opts := testOptions()
	n, span := int64(opts.MinSamples), opts.MinSpan
	nan := xidTrend(n, span, 400*time.Hour)
	nan.RatePerS = math.NaN()
	cases := []struct {
		name string
		tr   probes.RunwayTrend
		want string
	}{
		{"at the minimums", xidTrend(n, span, 400*time.Hour), ""},
		{"one sample fewer", xidTrend(n-1, span, 400*time.Hour), HoldUnmeasured},
		{"one second less", xidTrend(n, span-time.Second, 400*time.Hour), HoldUnmeasured},
		{"unknown rate", nan, HoldUnmeasured},
	}
	for _, c := range cases {
		in := EvalInput{Trends: []probes.RunwayTrend{c.tr}, TrendsOK: true,
			XID: xidReading(1.5e9, 2e8)} // past the freeze maximum (forecast-01)
		if got := heldReason(Holds(in, opts), CategoryWraparound, "xid"); got != c.want {
			t.Errorf("%s: held %q, want %q", c.name, got, c.want)
		}
	}
}

// Nil/empty: readable trends with no rows hold the cluster series (xid,
// mxid, disk), since nothing measured them.
func TestHolds_NoRowsHoldTheClusterSeries(t *testing.T) {
	h := Holds(EvalInput{TrendsOK: true}, testOptions())
	for _, c := range []struct{ cat, ident string }{{CategoryWraparound, "xid"},
		{CategoryWraparound, "mxid"}, {CategoryWAL, "disk"}} {
		if got := heldReason(h, c.cat, c.ident); got != HoldNoSamples {
			t.Errorf("%s/%s held %q, want %q (holds %v)", c.cat, c.ident, got,
				HoldNoSamples, h)
		}
	}
	if len(h[CategorySequence]) != 0 {
		t.Errorf("no sequence was read, yet holds %v", h[CategorySequence])
	}
}

// Error propagation: unreadable trends hold nothing (nothing is
// evaluated, so nothing resolves either).
func TestHolds_UnreadableTrendsHoldNothing(t *testing.T) {
	h := Holds(EvalInput{TrendsOK: false, SlotsOK: true,
		Slots: []probes.Slot{{Name: "cdc"}}}, testOptions())
	if len(h) != 0 {
		t.Fatalf("unreadable trends held %v", h)
	}
}

// A slot is held only while the current reading still lists it; a slot
// that is gone may resolve. Unreadable slots hold every unmeasured slot
// row (fail closed: it may still exist).
func TestHolds_SlotsFollowTheCurrentReading(t *testing.T) {
	opts := testOptions()
	short := trend(probes.RunwayWALSlot, "gone", 2, time.Minute, 5e9, 10<<30, 1, 1)
	live := trend(probes.RunwayWALSlot, "cdc", 2, time.Minute, 5e9, 10<<30, 1, 1)
	in := EvalInput{Trends: []probes.RunwayTrend{live, short}, TrendsOK: true,
		SlotsOK: true, Slots: []probes.Slot{{Name: "cdc"}, {Name: "fresh"}}}
	h := Holds(in, opts)
	if got := heldReason(h, CategoryWAL, "slot:cdc"); got != HoldUnmeasured {
		t.Errorf("listed unmeasured slot held %q, want %q", got, HoldUnmeasured)
	}
	if got := heldReason(h, CategoryWAL, "slot:fresh"); got != HoldNoSamples {
		t.Errorf("listed slot without samples held %q, want %q", got, HoldNoSamples)
	}
	if got, ok := h[CategoryWAL]["slot:gone"]; ok {
		t.Errorf("a dropped slot was held (%q); it must be free to resolve", got)
	}
	in.SlotsOK, in.Slots = false, nil
	h = Holds(in, opts)
	for _, ident := range []string{"slot:cdc", "slot:gone"} {
		if got := heldReason(h, CategoryWAL, ident); got != HoldUnmeasured {
			t.Errorf("unreadable slots: %s held %q, want %q", ident, got, HoldUnmeasured)
		}
	}
}

// A sequence is held only while the current reading lists it: one that
// was dropped or fell below the top N may resolve, and a cycling one
// never runs out.
func TestHolds_SequencesFollowTheCurrentReading(t *testing.T) {
	short := func(name string) probes.RunwayTrend {
		return trend(probes.RunwaySequence, name, 2, time.Minute, 2e9, 2147483647, 1, 1)
	}
	in := EvalInput{TrendsOK: true, SequencesOK: true,
		Trends: []probes.RunwayTrend{short("public.a_seq"), short("public.dropped_seq")},
		Sequences: []probes.SequenceRunway{{Sequence: "public.a_seq"},
			{Sequence: "public.new_seq"}, {Sequence: "public.loop_seq", Cycle: true}}}
	h := Holds(in, testOptions())
	want := map[string]string{"public.a_seq": HoldUnmeasured,
		"public.new_seq": HoldNoSamples}
	if len(h[CategorySequence]) != len(want) {
		t.Fatalf("sequence holds %v, want %v", h[CategorySequence], want)
	}
	for ident, reason := range want {
		if got := heldReason(h, CategorySequence, ident); got != reason {
			t.Errorf("%s held %q, want %q", ident, got, reason)
		}
	}
}

// truncatedTrends is a 200-row runway_trends result cut at the row cap:
// one measured sequence inside its horizon, then wal_position rows.
func truncatedTrends(truncated bool) probes.Result {
	first := evalAt.Add(-time.Hour)
	rows := []probes.Row{{"kind": probes.RunwaySequence, "subject": "public.a_seq",
		"samples": int64(12), "first_at": first, "last_at": evalAt, "last_value": 2e9,
		"last_limit": 2147483647.0, "r2": 1.0,
		"rate_per_s": rateFor(2e9, 2147483647, 10*24*time.Hour)}}
	for i := 1; len(rows) < 200; i++ {
		rows = append(rows, probes.Row{"kind": probes.RunwayWALPosition,
			"subject": fmt.Sprintf("s%03d", i), "samples": int64(12), "first_at": first,
			"last_at": evalAt, "last_value": 1.0, "last_limit": nil, "rate_per_s": 1.0,
			"r2": 1.0})
	}
	return probes.Result{ProbeID: probes.RunwayTrendsProbe, Version: "v2",
		Status: probes.StatusOK, Rows: rows, Truncated: truncated, ObservedAt: evalAt}
}

func truncatedInput(t *testing.T, truncated bool) EvalInput {
	t.Helper()
	set, err := probes.RunwayTrendsOf(truncatedTrends(truncated))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return EvalInput{Trends: set.Trends, TrendsOK: true, TrendsTruncated: set.Truncated,
		TrendsLastKind: set.LastKind, TablesOK: true, SequencesOK: true,
		Sequences: []probes.SequenceRunway{{Sequence: "public.a_seq"}}}
}

// Invalid input: a trends read cut at its row cap after wal_position
// leaves wraparound (xid) and WAL (wal_slot) unevaluated, while sequence
// (it sorts before wal_position) is evaluated and its runway still opens.
func TestEvaluate_TruncatedTrendsLeaveLaterCategories(t *testing.T) {
	rs, evaluated := Evaluate(truncatedInput(t, true), testOptions())
	if hasCategory(evaluated, CategoryWraparound) || hasCategory(evaluated, CategoryWAL) {
		t.Errorf("truncated trends evaluated %v; wraparound and WAL were cut", evaluated)
	}
	if !hasCategory(evaluated, CategorySequence) {
		t.Errorf("sequence sorts before the cut, yet evaluated only %v", evaluated)
	}
	if len(rs) != 1 || rs[0].Identifier != "public.a_seq" ||
		rs[0].Category != CategorySequence {
		t.Errorf("runways from the rows returned = %+v, want the sequence", rs)
	}
	_, evaluated = Evaluate(truncatedInput(t, false), testOptions())
	if len(evaluated) != 3 {
		t.Errorf("complete trends evaluated %v, want all three categories", evaluated)
	}
}

// A truncated read holds no cluster series it may have cut ("no_samples"
// would be a guess), and holds nothing at all when it returned no row.
func TestHolds_TruncatedTrendsGuessNothing(t *testing.T) {
	h := Holds(truncatedInput(t, true), testOptions())
	if _, ok := h[CategoryWraparound]["xid"]; ok {
		t.Errorf("xid was cut by the row cap, yet held as %q", h[CategoryWraparound]["xid"])
	}
	if got := heldReason(h, CategoryWraparound, "mxid"); got != HoldNoSamples {
		t.Errorf("mxid sorts before the cut and has no row: held %q, want %q", got,
			HoldNoSamples)
	}
	_, evaluated := Evaluate(EvalInput{TrendsOK: true, TrendsTruncated: true,
		TablesOK: true, SequencesOK: true}, testOptions())
	if len(evaluated) != 0 {
		t.Errorf("a truncated read with no row evaluated %v", evaluated)
	}
}
