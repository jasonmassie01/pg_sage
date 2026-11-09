package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-05: a descending sequence (a negative increment) counts down
// toward the higher of its MINVALUE and its owning column's minimum. The
// runway samples it negated, so the trend's limit is the negated limit;
// the diagnosis states the sequence's own values.

// No concurrent access tests: DiagnoseSequence is a pure function.

const int4Min = -2147483648.0

func downRow(last float64) probes.Row {
	limit := int4Min
	return probes.Row{"sequence": "public.d_seq", "data_type": "bigint",
		"increment_by": int64(-1), "cycle": false, "last_value": last,
		"min_value": -int8Max - 1, "max_value": int64(-1), "type_max": int8Max,
		"type_min": -int8Max - 1, "owner_column": "public.t.id",
		"owner_type_max": int4Max, "owner_type_min": int4Min,
		"effective_limit": limit, "fraction_used": (-1 - last) / (-1 - limit)}
}

func downCase(last, step, rate float64) []Observation {
	first := obs("E1", probes.SequenceRunwayProbe, downRow(last))
	first.Result.ObservedAt = t0
	after := obs("E3", probes.SequenceRunwayProbe, downRow(last-step))
	after.Result.ObservedAt = t0.Add(5 * time.Second)
	// The series is negated: -last_value rising toward -limit.
	trend := trendsObs("E2", trendRow(probes.RunwaySequence, "public.d_seq", 10,
		-(last-step), -int4Min, rate, 0.99))
	return []Observation{first, trend, after}
}

func TestSequence_DescendingNarrowColumnIsTheRoot(t *testing.T) {
	d := DiagnoseSequence(downCase(-2.1e9, 300, 60), "sequence public.d_seq")
	root := requireStatus(t, d, ColumnNarrowerThanSequence, StatusRoot)
	if !hasFact(root.Support, "E3", "-2147483648") {
		t.Fatalf("column root = %+v, want the column's minimum", root)
	}
	if !hasFact(root.Support, "E3", "advanced from -2100000000 to -2100000300") {
		t.Fatalf("column root = %+v, want the fall counted as consumption", root)
	}
	requireStatus(t, d, SequenceTypeLimit, StatusRuledOut)
}

// The trend's projection names the real (negative) limit and a fall.
func TestSequence_DescendingTrendStatesTheRealLimit(t *testing.T) {
	d := DiagnoseSequence(downCase(-1e9, 5000, 1000), "sequence public.d_seq")
	if !hasObserved(d, "E2", "falling at 1000 values per second", "limit -2147483648") {
		t.Fatalf("observed = %+v, want the fall toward -2147483648", d.Observed)
	}
	for _, f := range d.Observed {
		if strings.Contains(f.Text, "limit 2147483648") {
			t.Fatalf("observed %q states the negated series limit", f.Text)
		}
	}
}

// Boundary: a descending sequence that rose between the samples (setval
// upward) has not advanced; with a flat trend it is ruled out.
func TestSequence_DescendingThatRoseIsNotConsuming(t *testing.T) {
	d := DiagnoseSequence(downCase(-2.1e9, -300, 0), "sequence public.d_seq")
	if d.Root != nil {
		t.Fatalf("a rising descending sequence produced root %+v", d.Root)
	}
}
