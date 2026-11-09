package runway

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-01: freezing resets the XID (and multixact) age around
// autovacuum_freeze_max_age, so a cluster below it has an unbounded
// runway however fast it consumes XIDs. The wraparound trend opens a
// finding only at or past the effective freeze maximum (less the gate
// margin), projected with the net age slope when it is known and
// positive, else the consumption rate.
//
// No concurrency test: Evaluate and Holds are pure functions of their
// input and share nothing.

// xidReading is an xid_runway reading at XID age age under freeze
// maximum freezeMax (multixacts young, maximum 400M).
func xidReading(age, freezeMax float64) *probes.XIDRunway {
	return &probes.XIDRunway{NextXID: 4e9, MXIDCounter: 1e6, ClusterXIDAge: age,
		ClusterMXIDAge: 1, FreezeMaxAge: freezeMax, MXIDFreezeMaxAge: 4e8}
}

// mxidReading is an xid_runway reading at multixact age age under the
// multixact freeze maximum freezeMax (XIDs young, maximum 200M).
func mxidReading(age, freezeMax float64) *probes.XIDRunway {
	return &probes.XIDRunway{NextXID: 4e9, MXIDCounter: 1e6, ClusterXIDAge: 1,
		ClusterMXIDAge: age, FreezeMaxAge: 2e8, MXIDFreezeMaxAge: freezeMax}
}

// ageTrend is a measured series of kind at age, consuming consume per
// second, its age moving net per second.
func ageTrend(kind string, age, consume, net float64) probes.RunwayTrend {
	tr := trend(kind, probes.SubjectCluster, 12, time.Hour, age, WraparoundWarnAge,
		consume, 1)
	tr.ValueRatePerS = net
	return tr
}

func evalAge(x *probes.XIDRunway, tr probes.RunwayTrend,
	opts Options) ([]Runway, []string, map[string]map[string]string) {
	in := EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true, TablesOK: true,
		XID: x}
	rs, evaluated := Evaluate(in, opts)
	return rs, evaluated, Holds(in, opts)
}

// The plan's case: age 150M at 2,000 XIDs/s would reach the warning
// limit in about 11 days (inside the 14-day horizon) if freezing never
// ran; below autovacuum_freeze_max_age (200M) nothing opens, nothing is
// held, and the category is evaluated, so an open finding resolves.
func TestGate_XIDBelowFreezeMaxOpensNothing(t *testing.T) {
	opts := testOptions()
	tr := ageTrend(probes.RunwayXID, 1.5e8, 2000, 2000)
	if secs := tr.SecondsToLimit(); !within(secs, opts.WraparoundHorizon) {
		t.Fatalf("premise: the ungated projection %v s is outside the horizon", secs)
	}
	rs, evaluated, held := evalAge(xidReading(1.5e8, 2e8), tr, opts)
	if len(rs) != 0 {
		t.Fatalf("age 150M under freeze maximum 200M opened %+v", rs)
	}
	if r := heldReason(held, CategoryWraparound, "xid"); r != "" {
		t.Fatalf("a measured series below the gate was held (%q)", r)
	}
	if !hasCategory(evaluated, CategoryWraparound) {
		t.Fatalf("wraparound not evaluated (%v): its finding could not resolve", evaluated)
	}
}

// Age 210M, past the 200M maximum and rising: a warning, with the gate
// and the projection basis in its detail (CHECK-03) and in its title.
func TestGate_XIDPastFreezeMaxWarns(t *testing.T) {
	rs, _, held := evalAge(xidReading(2.1e8, 2e8),
		ageTrend(probes.RunwayXID, 2.1e8, 2000, 2000), testOptions())
	if len(rs) != 1 {
		t.Fatalf("age 210M past freeze maximum 200M: %d runways, want 1", len(rs))
	}
	r := rs[0]
	want := (WraparoundWarnAge - 2.1e8) / 2000
	if r.Severity != SeverityWarning || r.Identifier != "xid" ||
		r.Category != CategoryWraparound || math.Abs(r.SecondsToLimit-want) > 1 {
		t.Fatalf("runway = %+v, want a warning in %v s", r, want)
	}
	d := r.Finding().Detail
	for key, v := range map[string]any{"freeze_max_age": 2e8, "age": 2.1e8, "gate": 2e8,
		"projection_basis": BasisNetAgeSlope, "age_rate_per_s": 2000.0,
		"consumption_rate_per_s": 2000.0, "rate_per_s": 2000.0} {
		if d[key] != v {
			t.Errorf("detail[%s] = %v (%T), want %v", key, d[key], d[key], v)
		}
	}
	prefix := "XID age 210M is past autovacuum_freeze_max_age (200M) and rising; " +
		"the wraparound warning limit is in about "
	if !strings.HasPrefix(r.Title, prefix) || !strings.HasSuffix(r.Title, "m0s") {
		t.Errorf("title = %q, want prefix %q", r.Title, prefix)
	}
	if !strings.Contains(r.Recommendation, "autovacuum_freeze_max_age") {
		t.Errorf("recommendation does not name the setting: %q", r.Recommendation)
	}
	if r := heldReason(held, CategoryWraparound, "xid"); r != "" {
		t.Errorf("a projected series was held (%q)", r)
	}
}

// Boundary: the gate is inclusive (age == freeze maximum is projected,
// one less is not); a 0.05 margin moves it to 95% of the maximum.
func TestGate_XIDBoundaryAndMargin(t *testing.T) {
	margin := testOptions()
	margin.WraparoundGateMargin = 0.05
	cases := []struct {
		name string
		age  float64
		opts Options
		want bool
		gate float64
	}{
		{"at the maximum", 2e8, testOptions(), true, 2e8},
		{"one below the maximum", 199999999, testOptions(), false, 0},
		{"at the margin gate", 1.9e8, margin, true, 1.9e8},
		{"one below the margin gate", 189999999, margin, false, 0},
	}
	for _, c := range cases {
		rs, evaluated, held := evalAge(xidReading(c.age, 2e8),
			ageTrend(probes.RunwayXID, c.age, 10000, 10000), c.opts)
		if got := len(rs) == 1; got != c.want || len(rs) > 1 {
			t.Errorf("%s: runways %+v, want runway=%v", c.name, rs, c.want)
			continue
		}
		if c.want && (rs[0].Detail["gate"] != c.gate ||
			rs[0].Severity != SeverityCritical) {
			t.Errorf("%s: detail %v severity %s, want gate %v critical", c.name,
				rs[0].Detail, rs[0].Severity, c.gate)
		}
		if heldReason(held, CategoryWraparound, "xid") != "" ||
			!hasCategory(evaluated, CategoryWraparound) {
			t.Errorf("%s: held %v evaluated %v", c.name, held, evaluated)
		}
	}
}

// Past the gate the projection uses the net age slope when it is known
// and positive (500/s: a warning), else the consumption rate (2,000/s:
// critical), and records which.
func TestGate_ProjectionBasis(t *testing.T) {
	cases := []struct {
		name  string
		net   float64
		rate  float64
		basis string
		sev   string
	}{
		{"net slope 500/s", 500, 500, BasisNetAgeSlope, SeverityWarning},
		{"net slope 0", 0, 2000, BasisConsumption, SeverityCritical},
		{"net slope unknown", math.NaN(), 2000, BasisConsumption, SeverityCritical},
		{"net slope falling", -100, 2000, BasisConsumption, SeverityCritical},
	}
	for _, c := range cases {
		rs, _, _ := evalAge(xidReading(1.9e9, 2e8),
			ageTrend(probes.RunwayXID, 1.9e9, 2000, c.net), testOptions())
		if len(rs) != 1 {
			t.Errorf("%s: %d runways, want 1", c.name, len(rs))
			continue
		}
		r, want := rs[0], (WraparoundWarnAge-1.9e9)/c.rate
		if math.Abs(r.SecondsToLimit-want) > 1 || r.Severity != c.sev ||
			r.Detail["projection_basis"] != c.basis || r.Detail["rate_per_s"] != c.rate ||
			r.Detail["consumption_rate_per_s"] != 2000.0 {
			t.Errorf("%s: runway in %v s severity %s detail %v, want %v s %s basis %s",
				c.name, r.SecondsToLimit, r.Severity, r.Detail, want, c.sev, c.basis)
		}
		if c.basis == BasisConsumption && !strings.Contains(r.Title, "consumption") {
			t.Errorf("%s: title %q does not say it projects the consumption rate",
				c.name, r.Title)
		}
	}
}

// Nil/empty: without a reading, or with an unknown age or freeze
// maximum, whether freezing bounds the runway is unknown: no runway, and
// the xid series is held, so its open finding is kept (not resolvable).
func TestGate_UnknownReadingHoldsTheXIDFinding(t *testing.T) {
	nanMax, nanAge := xidReading(2.1e8, math.NaN()), xidReading(math.NaN(), 2e8)
	zeroMax := xidReading(2.1e8, 0)
	for name, x := range map[string]*probes.XIDRunway{"nil reading": nil,
		"unknown freeze maximum": nanMax, "unknown age": nanAge, "zero maximum": zeroMax} {
		rs, _, held := evalAge(x, ageTrend(probes.RunwayXID, 2.1e8, 1e5, 1e5),
			testOptions())
		if len(rs) != 0 {
			t.Errorf("%s: opened %+v", name, rs)
		}
		if r := heldReason(held, CategoryWraparound, "xid"); r != HoldFreezeGateUnknown {
			t.Errorf("%s: xid held %q, want %q", name, r, HoldFreezeGateUnknown)
		}
		if !keepOpen(nil, held, CategoryWraparound)["xid"] {
			t.Errorf("%s: the xid finding is not kept open", name)
		}
	}
	// An unmeasured series keeps its own reason; the gate is not consulted.
	short := ageTrend(probes.RunwayXID, 2.1e8, 1e5, 1e5)
	short.Samples = 3
	if _, _, held := evalAge(nil, short, testOptions()); heldReason(held,
		CategoryWraparound, "xid") != HoldUnmeasured {
		t.Errorf("an unmeasured series without a reading held %v", held)
	}
}

// State transition: a warning at 210M; after a freeze the measured age is
// 150M (its net slope falling), so the warning resolves: no runway, not
// held, wraparound evaluated.
func TestGate_FreezeResolvesAnXIDWarning(t *testing.T) {
	opts := testOptions()
	rs, _, _ := evalAge(xidReading(2.1e8, 2e8), ageTrend(probes.RunwayXID, 2.1e8, 2000,
		2000), opts)
	if len(rs) != 1 || rs[0].Severity != SeverityWarning {
		t.Fatalf("before the freeze: %+v, want one warning", rs)
	}
	rs, evaluated, held := evalAge(xidReading(1.5e8, 2e8),
		ageTrend(probes.RunwayXID, 1.5e8, 2000, -15000), opts)
	if len(rs) != 0 || heldReason(held, CategoryWraparound, "xid") != "" ||
		!hasCategory(evaluated, CategoryWraparound) {
		t.Fatalf("after the freeze: runways %+v held %v evaluated %v", rs, held, evaluated)
	}
	if keepOpen(nil, held, CategoryWraparound)["xid"] {
		t.Fatal("the frozen cluster's xid finding is kept open")
	}
}

// MXID: the same gate against autovacuum_multixact_freeze_max_age (400M).
func TestGate_MXIDAgainstMultixactFreezeMax(t *testing.T) {
	cases := []struct {
		name string
		age  float64
		want bool
	}{{"below", 3.5e8, false}, {"one below", 399999999, false}, {"at", 4e8, true},
		{"past", 4.1e8, true}}
	for _, c := range cases {
		rs, _, held := evalAge(mxidReading(c.age, 4e8),
			ageTrend(probes.RunwayMXID, c.age, 10000, 10000), testOptions())
		if got := len(rs) == 1; got != c.want || len(rs) > 1 {
			t.Errorf("%s: runways %+v, want runway=%v", c.name, rs, c.want)
			continue
		}
		if heldReason(held, CategoryWraparound, "mxid") != "" {
			t.Errorf("%s: mxid held %v", c.name, held)
		}
		if !c.want {
			continue
		}
		r := rs[0]
		if r.Identifier != "mxid" || r.Detail["freeze_max_age"] != 4e8 ||
			r.Detail["projection_basis"] != BasisNetAgeSlope {
			t.Errorf("%s: runway %+v", c.name, r)
		}
		if c.age == 4.1e8 && !strings.HasPrefix(r.Title, "Multixact age 410M is past "+
			"autovacuum_multixact_freeze_max_age (400M) and rising;") {
			t.Errorf("%s: title %q", c.name, r.Title)
		}
	}
	unknown := mxidReading(4.1e8, math.NaN())
	rs, _, held := evalAge(unknown, ageTrend(probes.RunwayMXID, 4.1e8, 1e4, 1e4),
		testOptions())
	if len(rs) != 0 || heldReason(held, CategoryWraparound, "mxid") != HoldFreezeGateUnknown {
		t.Errorf("unknown multixact maximum: runways %+v held %v", rs, held)
	}
}

// Invalid input: the gate margin is limited to 0-0.25 (and never NaN).
func TestOptions_WraparoundGateMarginRange(t *testing.T) {
	for _, c := range []struct {
		margin float64
		ok     bool
	}{{0, true}, {0.25, true}, {0.1, true}, {-0.01, false}, {0.2501, false},
		{math.NaN(), false}} {
		o := testOptions()
		o.Interval, o.Retention, o.WraparoundGateMargin = time.Minute, 48*time.Hour,
			c.margin
		err := o.validate()
		if (err == nil) != c.ok {
			t.Errorf("margin %v: err %v, want ok=%v", c.margin, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "gate margin") {
			t.Errorf("margin %v: error %q does not name the gate margin", c.margin, err)
		}
	}
}

// The recommendation reads as a sentence: the series is named in lower
// case mid-sentence, the consumption basis says what is consumed, and
// inside the gate margin (age still below the maximum) it does not claim
// autovacuum should already be freezing.
func TestGate_RecommendationWording(t *testing.T) {
	rs, _, _ := evalAge(mxidReading(4.1e8, 4e8),
		ageTrend(probes.RunwayMXID, 4.1e8, 10000, math.NaN()), testOptions())
	if len(rs) != 1 {
		t.Fatalf("mxid past the gate: %d runways, want 1", len(rs))
	}
	rec := rs[0].Recommendation
	for _, want := range []string{"Anti-wraparound autovacuum should already be freezing",
		"the multixact age is past autovacuum_multixact_freeze_max_age",
		"multixacts are consumed at 10000 per second"} {
		if !strings.Contains(rec, want) {
			t.Errorf("mxid recommendation %q lacks %q", rec, want)
		}
	}
	if strings.Contains(rec, "Multixact age") || strings.Contains(rec, "age is consumed") {
		t.Errorf("mxid recommendation %q is mis-worded", rec)
	}
	margin := testOptions()
	margin.WraparoundGateMargin = 0.05
	rs, _, _ = evalAge(xidReading(1.95e8, 2e8),
		ageTrend(probes.RunwayXID, 1.95e8, 10000, 10000), margin)
	if len(rs) != 1 {
		t.Fatalf("xid inside the margin: %d runways, want 1", len(rs))
	}
	rec = rs[0].Recommendation
	if strings.Contains(rec, "should already be freezing") ||
		!strings.Contains(rec, "the XID age is near autovacuum_freeze_max_age") ||
		!strings.Contains(rec, "it rises at 10000 per second") {
		t.Errorf("xid recommendation inside the margin: %q", rec)
	}
}
