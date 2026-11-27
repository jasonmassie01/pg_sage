package probes

import (
	"math"
	"testing"
	"time"
)

// forecast-01: RunwayTrend.ValueRatePerS is the net slope of a series'
// value (for xid and mxid, the age after freezing), decoded from
// value_rate_per_s; unknown (NULL or absent, as from a v2 row) is NaN,
// never zero.
//
// No concurrency test: the decoder is a pure function of its Result.
func TestRunwayTrends_DecodesTheValueRate(t *testing.T) {
	at := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	row := func(v any, set bool) Row {
		r := Row{"kind": RunwayXID, "subject": SubjectCluster, "samples": int64(12),
			"first_at": at, "last_at": at.Add(time.Hour), "last_value": 2.1e8,
			"last_limit": 2107483647.0, "rate_per_s": 2000.0, "r2": 1.0}
		if set {
			r["value_rate_per_s"] = v
		}
		return r
	}
	cases := []struct {
		name string
		row  Row
		want float64
	}{
		{"known", row(500.0, true), 500},
		{"negative (freezing ran)", row(-40.0, true), -40},
		{"zero", row(0.0, true), 0},
		{"null", row(nil, true), math.NaN()},
		{"absent (a v2 row)", row(nil, false), math.NaN()},
	}
	for _, c := range cases {
		ts, err := RunwayTrends(okResult(RunwayTrendsProbe, nil, c.row))
		if err != nil || len(ts) != 1 {
			t.Fatalf("%s: trends %+v (%v)", c.name, ts, err)
		}
		got := ts[0].ValueRatePerS
		if math.IsNaN(c.want) != math.IsNaN(got) || (!math.IsNaN(c.want) && got != c.want) {
			t.Errorf("%s: value rate %v, want %v", c.name, got, c.want)
		}
		if ts[0].RatePerS != 2000 {
			t.Errorf("%s: consumption rate %v, want 2000 (unchanged)", c.name,
				ts[0].RatePerS)
		}
	}
}
