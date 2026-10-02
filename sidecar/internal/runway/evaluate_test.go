package runway

import (
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// A runway crosses its horizon when a trend measured over enough samples
// and time projects its limit inside it (critical inside the critical
// horizon), or when a table is overdue for freezing. Nothing is
// projected from too few samples, an unknown limit or a sawtooth.

func testOptions() Options {
	return Options{Database: "orders", Investigate: true, MinSamples: 10,
		MinSpan: 30 * time.Minute, WraparoundHorizon: 336 * time.Hour,
		WraparoundCritical: 72 * time.Hour, DiskHorizon: 72 * time.Hour,
		DiskCritical: 24 * time.Hour, SequenceHorizon: 30 * 24 * time.Hour,
		SequenceCritical: 7 * 24 * time.Hour, Lookback: 6 * time.Hour}
}

var evalAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// trend is a series measured over span with n samples ending at evalAt.
func trend(kind, subject string, n int64, span time.Duration, last, limit, rate,
	r2 float64) probes.RunwayTrend {
	return probes.RunwayTrend{Kind: kind, Subject: subject, Samples: n,
		FirstAt: evalAt.Add(-span), LastAt: evalAt, LastValue: last, Limit: limit,
		RatePerS: rate, R2: r2}
}

// secondsAt returns the rate that reaches limit from last in d.
func rateFor(last, limit float64, d time.Duration) float64 {
	return (limit - last) / d.Seconds()
}

func evalOne(t *testing.T, tr probes.RunwayTrend) []Runway {
	t.Helper()
	rs, _ := Evaluate(EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true},
		testOptions())
	return rs
}

func TestEvaluate_XIDRunwaySeverities(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{{71 * time.Hour, SeverityCritical}, {72 * time.Hour, SeverityCritical},
		{200 * time.Hour, SeverityWarning}, {336 * time.Hour, SeverityWarning},
		{337 * time.Hour, ""}}
	for _, c := range cases {
		rate := rateFor(1.5e9, WraparoundWarnAge, c.in)
		rs := evalOne(t, trend(probes.RunwayXID, probes.SubjectCluster, 12, time.Hour,
			1.5e9, WraparoundWarnAge, rate, 1))
		if c.want == "" {
			if len(rs) != 0 {
				t.Errorf("runway in %s produced %+v", c.in, rs)
			}
			continue
		}
		if len(rs) != 1 {
			t.Fatalf("runway in %s produced %d runways", c.in, len(rs))
		}
		r := rs[0]
		if r.Severity != c.want || r.Category != CategoryWraparound ||
			r.Identifier != "xid" || r.Kind != sre.TriggerWraparound || r.Subject != "xid" ||
			math.Abs(r.SecondsToLimit-c.in.Seconds()) > 1 {
			t.Errorf("runway in %s = %+v", c.in, r)
		}
	}
}

// A trend needs MinSamples samples over at least MinSpan.
func TestEvaluate_TrendMinimums(t *testing.T) {
	rate := rateFor(1.5e9, WraparoundWarnAge, 10*time.Hour)
	for _, c := range []struct {
		n    int64
		span time.Duration
		want int
	}{{10, 30 * time.Minute, 1}, {9, time.Hour, 0}, {12, 29 * time.Minute, 0}} {
		rs := evalOne(t, trend(probes.RunwayXID, probes.SubjectCluster, c.n, c.span,
			1.5e9, WraparoundWarnAge, rate, 1))
		if len(rs) != c.want {
			t.Errorf("n=%d span=%s: %d runways, want %d", c.n, c.span, len(rs), c.want)
		}
	}
}

func TestEvaluate_OverdueTables(t *testing.T) {
	for _, c := range []struct {
		xid  float64
		want string
	}{{124999, ""}, {125000, SeverityWarning}, {199999, SeverityWarning},
		{200000, SeverityCritical}} {
		rs, _ := Evaluate(EvalInput{TrendsOK: true, TablesOK: true,
			Tables: []probes.WraparoundTable{{Relation: "public.events", XIDAge: c.xid,
				FreezeMaxAge: 100000, MXIDAge: 1, MXIDFreezeMaxAge: 4e8}}}, testOptions())
		if c.want == "" {
			if len(rs) != 0 {
				t.Errorf("age %v: %+v", c.xid, rs)
			}
			continue
		}
		if len(rs) != 1 || rs[0].Severity != c.want || rs[0].Identifier != "public.events" ||
			rs[0].Subject != "table public.events" || rs[0].ObjectType != "table" ||
			!math.IsNaN(rs[0].SecondsToLimit) {
			t.Errorf("age %v: %+v", c.xid, rs)
		}
	}
}

func TestEvaluate_DiskAndSlots(t *testing.T) {
	disk := trend(probes.RunwayDiskUsed, probes.SubjectCluster, 12, time.Hour, 9e10, 1e11,
		rateFor(9e10, 1e11, 30*time.Hour), 0.9)
	rs := evalOne(t, disk)
	if len(rs) != 1 || rs[0].Identifier != "disk" || rs[0].Severity != SeverityWarning ||
		rs[0].Kind != sre.TriggerDiskWAL || rs[0].Category != CategoryWAL {
		t.Fatalf("disk runway = %+v", rs)
	}
	sawtooth := disk
	sawtooth.R2 = 0.49
	unknown := disk
	unknown.Limit = math.NaN()
	for name, tr := range map[string]probes.RunwayTrend{"sawtooth": sawtooth,
		"undeclared capacity": unknown} {
		if rs := evalOne(t, tr); len(rs) != 0 {
			t.Errorf("%s produced %+v", name, rs)
		}
	}
	slot := trend(probes.RunwayWALSlot, "cdc", 12, time.Hour, 5e9, 10<<30,
		rateFor(5e9, 10<<30, 16*time.Hour), 0.95)
	rs = evalOne(t, slot)
	if len(rs) != 1 || rs[0].Identifier != "slot:cdc" || rs[0].Subject != "slot cdc" ||
		rs[0].Severity != SeverityCritical || rs[0].ObjectType != "replication_slot" {
		t.Fatalf("slot runway = %+v", rs)
	}
}

func TestEvaluate_Sequences(t *testing.T) {
	seq := trend(probes.RunwaySequence, "public.a_seq", 12, time.Hour, 2e9, 2147483647,
		rateFor(2e9, 2147483647, 17*24*time.Hour), 1)
	rs, _ := Evaluate(EvalInput{Trends: []probes.RunwayTrend{seq}, TrendsOK: true,
		SequencesOK: true, Sequences: []probes.SequenceRunway{{Sequence: "public.a_seq"}}},
		testOptions())
	if len(rs) != 1 || rs[0].Severity != SeverityWarning ||
		rs[0].Subject != "sequence public.a_seq" || rs[0].Kind != sre.TriggerSequence ||
		rs[0].Category != CategorySequence || rs[0].ObjectType != "sequence" {
		t.Fatalf("sequence runway = %+v", rs)
	}
	rs, _ = Evaluate(EvalInput{Trends: []probes.RunwayTrend{seq}, TrendsOK: true,
		SequencesOK: true, Sequences: []probes.SequenceRunway{{Sequence: "public.a_seq",
			Cycle: true}}}, testOptions())
	if len(rs) != 0 {
		t.Fatalf("a cycling sequence produced %+v", rs)
	}
}

// Categories are evaluated (their cleared findings resolved) only when
// their inputs were read.
func TestEvaluate_EvaluatedCategories(t *testing.T) {
	cases := []struct {
		in   EvalInput
		want []string
	}{
		{EvalInput{TrendsOK: true, TablesOK: true, SequencesOK: true},
			[]string{CategoryWraparound, CategoryWAL, CategorySequence}},
		{EvalInput{TrendsOK: true}, []string{CategoryWAL}},
		{EvalInput{TablesOK: true, SequencesOK: true}, nil},
	}
	for i, c := range cases {
		_, got := Evaluate(c.in, testOptions())
		if len(got) != len(c.want) {
			t.Fatalf("case %d evaluated %v, want %v", i, got, c.want)
		}
		for j := range got {
			if got[j] != c.want[j] {
				t.Fatalf("case %d evaluated %v, want %v", i, got, c.want)
			}
		}
	}
}

func TestRunway_FindingCarriesTheProjection(t *testing.T) {
	rs := evalOne(t, trend(probes.RunwaySequence, "public.a_seq", 12, time.Hour, 2e9,
		2147483647, rateFor(2e9, 2147483647, 48*time.Hour), 1))
	f := rs[0].Finding()
	if f.Category != CategorySequence || f.ObjectIdentifier != "public.a_seq" ||
		f.Severity != SeverityCritical || f.Title == "" || f.Recommendation == "" ||
		f.RecommendedSQL != "" || f.DatabaseName != "" {
		t.Fatalf("finding = %+v", f)
	}
	for _, key := range []string{"runway_kind", "seconds_to_limit", "rate_per_s", "limit",
		"last_value", "samples", "span_s", "r2", "horizon_s"} {
		if _, ok := f.Detail[key]; !ok {
			t.Errorf("detail lacks %s: %v", key, f.Detail)
		}
	}
}
