package runway

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-05: the runway is the single sequence forecast. A descending
// sequence is sampled negated (-last_value toward -limit), so the same
// projection serves both directions; its finding shows the real values.
// The horizons are inclusive: 7 days is critical, 30 days opens.

// No concurrent access tests: BuildSamples and Evaluate are pure
// functions of their inputs.

func seqSnapshot(ss ...probes.SequenceRunway) Snapshot {
	return Snapshot{Sequences: ss, SequencesOK: true, SequencesFresh: true}
}

func TestBuildSamples_DescendingSequencesAreNegated(t *testing.T) {
	down := probes.SequenceRunway{Sequence: "public.down", Increment: -1,
		LastValue: -2100000000, MinValue: -9.2e18, MaxValue: -1, Limit: -2147483648}
	up := probes.SequenceRunway{Sequence: "public.up", Increment: 1, LastValue: 2100000000,
		MinValue: 1, MaxValue: 2147483647, Limit: 2147483647}
	cyc := probes.SequenceRunway{Sequence: "public.cyc", Increment: -1, Cycle: true,
		LastValue: -100, Limit: -32768}
	got := byKey(BuildSamples(seqSnapshot(down, up, cyc), testOptions()))
	d, ok := got[probes.RunwaySequence+"/public.down"]
	if !ok || d.Value != 2100000000 || d.Counter != 2100000000 || d.Limit != 2147483648 {
		t.Fatalf("descending sample = %+v (%v), want value and counter 2100000000 "+
			"toward 2147483648", d, ok)
	}
	u, ok := got[probes.RunwaySequence+"/public.up"]
	if !ok || u.Value != 2100000000 || u.Counter != 2100000000 || u.Limit != 2147483647 {
		t.Fatalf("ascending sample = %+v (%v), want it unchanged", u, ok)
	}
	if c, ok := got[probes.RunwaySequence+"/public.cyc"]; ok {
		t.Fatalf("a cycling descending sequence was sampled: %+v", c)
	}
	// Each nextval moves a descending sequence's counter up: one epoch.
	last := map[seriesKey]lastPoint{}
	first := assignEpochs([]Sample{d}, last, evalAt)
	d2 := d
	d2.Value, d2.Counter = d.Value+500, d.Counter+500
	next := assignEpochs([]Sample{d2}, last, evalAt.Add(time.Minute))
	if first[0].Epoch != next[0].Epoch {
		t.Fatalf("a descending sequence's next value started a new epoch")
	}
}

// Nil/empty: no sequences is an evaluated, empty reading.
func TestEvaluate_NoSequencesOpensNothing(t *testing.T) {
	if n := len(BuildSamples(seqSnapshot(), testOptions())); n != 0 {
		t.Fatalf("no sequences built %d samples", n)
	}
	rs, evaluated := Evaluate(EvalInput{TrendsOK: true, SequencesOK: true}, testOptions())
	if len(rs) != 0 || !contains(evaluated, CategorySequence) {
		t.Fatalf("no sequences = %+v evaluated %v, want none and the category evaluated",
			rs, evaluated)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestEvaluate_SequenceHorizonBoundaries(t *testing.T) {
	const day = 86400.0
	cases := []struct {
		days float64
		want string
	}{{7.0, SeverityCritical}, {7.01, SeverityWarning}, {30.0, SeverityWarning},
		{30.01, ""}, {0.5, SeverityCritical}}
	for _, c := range cases {
		tr := trend(probes.RunwaySequence, "public.edge", 12, time.Hour, 0, c.days*day, 1, 1)
		rs, _ := Evaluate(EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true,
			SequencesOK: true, Sequences: []probes.SequenceRunway{{Sequence: "public.edge",
				Increment: 1}}}, testOptions())
		switch {
		case c.want == "" && len(rs) != 0:
			t.Errorf("%.2f days opened %+v, want nothing past the horizon", c.days, rs)
		case c.want != "" && (len(rs) != 1 || rs[0].Severity != c.want):
			t.Errorf("%.2f days = %+v, want one %s runway", c.days, rs, c.want)
		}
	}
}

func TestEvaluate_DescendingSequenceShowsItsRealValues(t *testing.T) {
	const name = "public.down"
	tr := trend(probes.RunwaySequence, name, 12, time.Hour, 2100000000, 2147483648,
		rateFor(2100000000, 2147483648, 10*24*time.Hour), 1)
	in := EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true, SequencesOK: true,
		Sequences: []probes.SequenceRunway{{Sequence: name, Increment: -1,
			LastValue: -2100000000, Limit: -2147483648}}}
	rs, _ := Evaluate(in, testOptions())
	if len(rs) != 1 || rs[0].Category != CategorySequence || rs[0].Identifier != name ||
		rs[0].Severity != SeverityWarning ||
		math.Abs(rs[0].SecondsToLimit-10*86400) > 1 {
		t.Fatalf("descending runway = %+v", rs)
	}
	d := rs[0].Detail
	if d["limit"] != -2147483648.0 || d["last_value"] != -2100000000.0 ||
		d["direction"] != "descending" {
		t.Fatalf("detail = %v, want the sequence's own (negative) values", d)
	}
	if !strings.Contains(rs[0].Recommendation, "falls") ||
		strings.Contains(rs[0].Recommendation, "grows") {
		t.Fatalf("recommendation %q, want a falling sequence described", rs[0].Recommendation)
	}
	in.Sequences[0].Cycle = true
	if rs, _ := Evaluate(in, testOptions()); len(rs) != 0 {
		t.Fatalf("a cycling descending sequence opened %+v", rs)
	}
}
