package runway

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The wraparound gate (forecast-01). Anti-wraparound autovacuum freezes
// a table once its age passes autovacuum_freeze_max_age, which resets
// the cluster age: below that maximum the runway is bounded by freezing,
// however fast XIDs are consumed. So an xid (mxid) trend is projected
// only at or past the effective autovacuum_freeze_max_age
// (autovacuum_multixact_freeze_max_age), less the gate margin. Below it
// nothing opens; stuck xmin holders there stay with the xmin-horizon
// probe and the overdue-table rule.

// HoldFreezeGateUnknown: the series is measured, but the cluster age or
// its freeze maximum is unknown (no xid_runway reading this tick), so
// whether freezing bounds the runway is unknown: the finding is held.
const HoldFreezeGateUnknown = "freeze_gate_unknown"

// Projection bases of a wraparound trend past its gate.
const (
	// BasisNetAgeSlope: the age's own slope, freezing included.
	BasisNetAgeSlope = "net_age_slope"
	// BasisConsumption: the XID (multixact) consumption rate, used when
	// the net slope is unknown or not rising (a conservative projection).
	BasisConsumption = "consumption_rate"
)

// MaxWraparoundGateMargin bounds Options.WraparoundGateMargin.
const MaxWraparoundGateMargin = 0.25

// ageGate is one series' gate: the cluster age, its effective freeze
// maximum, the age at or past which the trend is projected, the setting
// that maximum comes from, and how the series and what it consumes are
// named (what heads a title, noun goes mid-sentence).
type ageGate struct {
	what, noun, unit, setting string
	age, freezeMax, gate      float64
}

// wraparoundGate returns the gate of an xid or mxid series; false when
// the reading is missing or its age or freeze maximum is unknown.
func wraparoundGate(kind string, x *probes.XIDRunway, margin float64) (ageGate, bool) {
	if x == nil {
		return ageGate{}, false
	}
	g := ageGate{what: "XID age", noun: "XID age", unit: "XIDs",
		setting: "autovacuum_freeze_max_age", age: x.ClusterXIDAge, freezeMax: x.FreezeMaxAge}
	if kind == probes.RunwayMXID {
		g = ageGate{what: "Multixact age", noun: "multixact age", unit: "multixacts",
			setting: "autovacuum_multixact_freeze_max_age", age: x.ClusterMXIDAge,
			freezeMax: x.MXIDFreezeMaxAge}
	}
	if !probes.Known(g.age) || !(g.freezeMax > 0) {
		return ageGate{}, false
	}
	g.gate = g.freezeMax - g.freezeMax*margin
	return g, true
}

// ageProjection is the rate a series past its gate is projected with:
// the net age slope when it is known and rising, else the consumption
// rate.
func ageProjection(tr probes.RunwayTrend) (float64, string) {
	if probes.Known(tr.ValueRatePerS) && tr.ValueRatePerS > 0 {
		return tr.ValueRatePerS, BasisNetAgeSlope
	}
	return tr.RatePerS, BasisConsumption
}

// ageRunway projects a measured xid or mxid trend that is past its gate.
func ageRunway(tr probes.RunwayTrend, in EvalInput, opts Options) (Runway, bool) {
	g, ok := wraparoundGate(tr.Kind, in.XID, opts.WraparoundGateMargin)
	if !ok || !(g.age >= g.gate) {
		return Runway{}, false
	}
	rate, basis := ageProjection(tr)
	p := tr
	p.RatePerS = rate
	r := Runway{Category: CategoryWraparound, ObjectType: "database", Identifier: tr.Kind,
		Kind: sre.TriggerWraparound, Subject: tr.Kind}
	if !project(&r, p, opts.WraparoundHorizon, opts.WraparoundCritical) {
		return Runway{}, false
	}
	r.Detail = trendDetail(p, opts.WraparoundHorizon)
	r.Detail["freeze_max_age"], r.Detail["age"] = g.freezeMax, g.age
	r.Detail["gate"], r.Detail["projection_basis"] = round2(g.gate), basis
	r.Detail["age_rate_per_s"] = finiteOrNil(tr.ValueRatePerS)
	r.Detail["consumption_rate_per_s"] = finiteOrNil(tr.RatePerS)
	r.Title, r.Recommendation = ageWords(r, p, g, basis)
	return r, true
}

// ageWords titles a wraparound runway past its gate.
func ageWords(r Runway, p probes.RunwayTrend, g ageGate, basis string) (string, string) {
	when := (time.Duration(r.SecondsToLimit) * time.Second).Round(time.Minute)
	where, lead := "is past", "Anti-wraparound autovacuum should already be freezing"
	if g.age < g.freezeMax { // inside the gate margin: freezing has not started
		where, lead = "is near", "Anti-wraparound autovacuum is about to start freezing"
	}
	head := fmt.Sprintf("%s %s %s %s (%s)", g.what, approxCount(g.age), where, g.setting,
		approxCount(g.freezeMax))
	title := fmt.Sprintf("%s and rising; the wraparound warning limit is in about %s",
		head, when)
	if basis == BasisConsumption {
		title = fmt.Sprintf("%s; at its consumption rate the wraparound warning limit "+
			"is in about %s", head, when)
	}
	pace := "it rises"
	if basis == BasisConsumption {
		pace = g.unit + " are consumed"
	}
	rec := fmt.Sprintf("%s: the %s %s %s, and %s at %s per second (%s, measured over %d "+
		"samples). A read-only pre-incident investigation looks for what holds freezing "+
		"back; act before the wraparound warning limit is reached.", lead, g.noun, where,
		g.setting, pace, probes.FormatValue(round2(p.RatePerS)), basis, p.Samples)
	return title, rec
}

// approxCount writes a large count briefly: 210M, 2.1B.
func approxCount(v float64) string {
	trim := func(f float64, unit string) string {
		return strconv.FormatFloat(f, 'f', -1, 64) + unit
	}
	switch {
	case v >= 1e9:
		return trim(float64(int64(v/1e7+0.5))/100, "B")
	case v >= 1e6:
		return trim(float64(int64(v/1e5+0.5))/10, "M")
	}
	return probes.FormatValue(round2(v))
}
