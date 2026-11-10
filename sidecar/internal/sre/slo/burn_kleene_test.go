package slo

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Kleene AND for multi-window burn-rate rules (playbooks-07): a rule
// whose known window is below the factor is decidably not firing; a rule
// stays unknown while a known window could still fire, while no window
// is known, and while the unknown window's data is absent, stale, failed
// or invalid. A firing rule still beats an unknown one.
//
// No concurrent access tests: Evaluate is a pure function of its inputs.

// withUnknown sets a window's unknown reason.
func withUnknown(ws map[time.Duration]Window, d time.Duration, reason string) {
	w := ws[d]
	w.Unknown = reason
	ws[d] = w
}

// The long window could still fire (20x >= 14.4x), so a sparse short
// window keeps the page rule, and the SLO, unknown.
func TestEvaluate_LongBurningShortSparseStaysUnknown(t *testing.T) {
	ws := windowsAt(allBurning(0.1))
	ws[time.Hour] = known(time.Hour, 20)
	withUnknown(ws, 5*time.Minute, ReasonLowTraffic)
	st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	r := st.Rules[0]
	if st.State != StateUnknown || st.FastBurning ||
		strings.Join(st.Unknown, ",") != ReasonLowTraffic {
		t.Fatalf("status = %s %v, want unknown low_traffic", st.State, st.Unknown)
	}
	if r.Firing || r.Unknown != ReasonLowTraffic || r.Partial != "" {
		t.Fatalf("rule = %+v", r)
	}
}

// The floor is inclusive: a long window exactly at the factor could
// fire, so the rule stays unknown; just under the factor decides it.
func TestEvaluate_KleeneFloorBoundary(t *testing.T) {
	for _, c := range []struct {
		burn    float64
		state   State
		unknown string
		partial string
	}{
		{14.4, StateUnknown, ReasonLowTraffic, ""},
		{14.39, StateOK, "", ReasonLowTraffic},
		{0, StateOK, "", ReasonLowTraffic},
	} {
		ws := windowsAt(allBurning(0.1))
		ws[time.Hour] = known(time.Hour, c.burn)
		withUnknown(ws, 5*time.Minute, ReasonLowTraffic)
		st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
		r := st.Rules[0]
		if st.State != c.state || r.Unknown != c.unknown || r.Partial != c.partial ||
			r.Firing {
			t.Errorf("long %v: state %s rule %+v, want %s unknown %q partial %q",
				c.burn, st.State, r, c.state, c.unknown, c.partial)
		}
	}
}

// The rule is symmetric: a quiet short window decides the rule when the
// long one is sparse (a new SLI whose long window is not yet covered).
func TestEvaluate_QuietShortDecidesSparseLong(t *testing.T) {
	ws := windowsAt(allBurning(0.1))
	withUnknown(ws, time.Hour, ReasonPartialWindow)
	st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	r := st.Rules[0]
	if st.State != StateOK || r.Firing || r.Unknown != "" ||
		r.Partial != ReasonPartialWindow || r.Long.BurnRate != nil {
		t.Fatalf("status = %s rule = %+v", st.State, r)
	}
}

// State transitions: both windows stale, or both sparse, leave nothing
// known, so the rule and the SLO are unknown, never ok.
func TestEvaluate_NoKnownWindowStaysUnknown(t *testing.T) {
	for _, reason := range []string{ReasonStale, ReasonLowTraffic} {
		ws := windowsAt(allBurning(0.1))
		withUnknown(ws, time.Hour, reason)
		withUnknown(ws, 5*time.Minute, reason)
		st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
		r := st.Rules[0]
		if st.State != StateUnknown || r.Unknown != reason || r.Partial != "" || r.Firing {
			t.Errorf("both %s: state %s rule %+v", reason, st.State, r)
		}
	}
}

// A stale source never turns ok: a stale window keeps the rule unknown
// even beside a quiet known window.
func TestEvaluate_StaleNeverDecidedByOtherWindow(t *testing.T) {
	ws := windowsAt(allBurning(0.1))
	withUnknown(ws, time.Hour, ReasonStale)
	st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	if st.State != StateUnknown || st.Rules[0].Unknown != ReasonStale ||
		st.Rules[0].Partial != "" {
		t.Fatalf("status = %s rule = %+v", st.State, st.Rules[0])
	}
}

// A firing rule still beats an unknown one, and a decided rule does not
// hide another rule's unknown.
func TestEvaluate_KleeneKeepsPrecedence(t *testing.T) {
	ws := windowsAt(allBurning(16))
	withUnknown(ws, 72*time.Hour, ReasonStale)
	st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 1), now)
	if st.State != StatePage || !st.FastBurning || st.Rules[2].Unknown != ReasonStale {
		t.Fatalf("firing over unknown: %s %+v", st.State, st.Rules)
	}
	ws = windowsAt(allBurning(0.1))
	withUnknown(ws, 5*time.Minute, ReasonLowTraffic)
	withUnknown(ws, 30*time.Minute, ReasonNoData)
	st = Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	if st.State != StateUnknown || strings.Join(st.Unknown, ",") != ReasonNoData ||
		st.Rules[0].Partial != ReasonLowTraffic || st.Rules[1].Unknown != ReasonNoData {
		t.Fatalf("decided beside unknown: %s %v %+v", st.State, st.Unknown, st.Rules)
	}
}

// A database proxy gets the same rule logic.
func TestEvaluate_KleeneAppliesToProxies(t *testing.T) {
	o := appObjective()
	o.Kind, o.Source, o.Proxy = KindProxy, SourceProxy, "db_latency"
	ws := windowsAt(allBurning(0.1))
	withUnknown(ws, 5*time.Minute, ReasonLowTraffic)
	st := Evaluate(o, DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	if st.State != StateOK || st.Rules[0].Partial != ReasonLowTraffic || st.CustomerImpact {
		t.Fatalf("proxy status = %s rule %+v", st.State, st.Rules[0])
	}
}

// The partial reason reaches surfaces as "partial"; a decided rule has
// no "unknown" key.
func TestRuleResult_PartialJSON(t *testing.T) {
	ws := windowsAt(allBurning(0.1))
	withUnknown(ws, 5*time.Minute, ReasonLowTraffic)
	st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	raw, err := json.Marshal(st.Rules[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["partial"] != ReasonLowTraffic || got["firing"] != false {
		t.Fatalf("rule json = %s", raw)
	}
	if _, has := got["unknown"]; has {
		t.Fatalf("decided rule carries unknown: %s", raw)
	}
}
