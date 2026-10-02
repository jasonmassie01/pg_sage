package slo

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Multi-window, multi-burn-rate evaluation (AI-SRE-SPEC §8, Google SRE
// workbook defaults): page at 14.4x over 1h and 5m, or 6x over 6h and
// 30m; ticket at 1x over 3d and 6h. A rule fires only when both of its
// windows burn at or above its factor. Low traffic, a zero denominator
// and absent data are unknown, never "ok".

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func appObjective() Objective {
	return Objective{Name: "checkout", Kind: KindApp, Source: SourcePush,
		Target: 0.999, Window: 30 * 24 * time.Hour, MinEligible: 100,
		StaleAfter: 5 * time.Minute}
}

// known is a window with burn rate burn at target 0.999 over 100000
// eligible events.
func known(d time.Duration, burn float64) Window {
	return Window{Duration: d, Eligible: 100000, Bad: burn * 0.001 * 100000,
		Coverage: 1, LatestAt: now}
}

func windowsAt(burns map[time.Duration]float64) map[time.Duration]Window {
	out := map[time.Duration]Window{}
	for d, b := range burns {
		out[d] = known(d, b)
	}
	return out
}

func allBurning(b float64) map[time.Duration]float64 {
	return map[time.Duration]float64{5 * time.Minute: b, 30 * time.Minute: b,
		time.Hour: b, 6 * time.Hour: b, 72 * time.Hour: b}
}

func TestDefaultRules_AreTheWorkbookDefaults(t *testing.T) {
	want := []Rule{{SeverityPage, time.Hour, 5 * time.Minute, 14.4},
		{SeverityPage, 6 * time.Hour, 30 * time.Minute, 6},
		{SeverityTicket, 72 * time.Hour, 6 * time.Hour, 1}}
	got := DefaultRules()
	if len(got) != len(want) {
		t.Fatalf("rules = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := ValidateRules(got); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	got[0].Factor = 1
	if DefaultRules()[0].Factor != 14.4 {
		t.Fatal("DefaultRules returns shared state")
	}
}

func TestValidateRules_Invalid(t *testing.T) {
	cases := map[string][]Rule{
		"none":          {},
		"bad severity":  {{"warn", time.Hour, time.Minute, 2}},
		"zero factor":   {{SeverityPage, time.Hour, time.Minute, 0}},
		"nan factor":    {{SeverityPage, time.Hour, time.Minute, math.NaN()}},
		"short >= long": {{SeverityPage, time.Hour, time.Hour, 2}},
		"zero short":    {{SeverityPage, time.Hour, 0, 2}},
		"too long":      {{SeverityTicket, 91 * 24 * time.Hour, time.Hour, 1}},
	}
	for name, rs := range cases {
		if err := ValidateRules(rs); err == nil {
			t.Errorf("%s: ValidateRules accepted %+v", name, rs)
		}
	}
}

func TestWindow_BurnRate(t *testing.T) {
	w := Window{Duration: time.Hour, Bad: 30, Eligible: 1000, Coverage: 1}
	b, ok := w.BurnRate(0.99)
	if !ok || math.Abs(b-3) > 1e-9 {
		t.Fatalf("burn = %v ok=%v, want 3", b, ok)
	}
	w.Unknown = ReasonLowTraffic
	if _, ok := w.BurnRate(0.99); ok || w.Known() {
		t.Fatal("an unknown window has a burn rate")
	}
	zero := Window{Duration: time.Hour, Coverage: 1}
	if _, ok := zero.BurnRate(0.99); ok {
		t.Fatal("a zero-denominator window has a burn rate")
	}
}

func TestEvaluate_FastBurnPages(t *testing.T) {
	st := Evaluate(appObjective(), DefaultRules(), windowsAt(allBurning(15)),
		known(30*24*time.Hour, 2), now)
	if st.State != StatePage || !st.FastBurning || !st.CustomerImpact {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Rules) != 3 || !st.Rules[0].Firing || !st.Rules[1].Firing ||
		!st.Rules[2].Firing {
		t.Fatalf("rules = %+v", st.Rules)
	}
	r := st.Rules[0]
	if r.Long.Window != "1h" || r.Short.Window != "5m" || r.Long.BurnRate == nil ||
		math.Abs(*r.Long.BurnRate-15) > 1e-9 || r.Factor != 14.4 {
		t.Fatalf("page rule = %+v", r)
	}
	if st.Name != "checkout" || st.Kind != KindApp || st.Target != 0.999 ||
		!st.EvaluatedAt.Equal(now) || len(st.Unknown) != 0 {
		t.Fatalf("identity = %+v", st)
	}
}

// Both windows must burn: a long-window burn that the short window no
// longer shows does not page (it has recovered), but may still ticket.
func TestEvaluate_BothWindowsRequired(t *testing.T) {
	burns := map[time.Duration]float64{time.Hour: 20, 5 * time.Minute: 2,
		6 * time.Hour: 4, 30 * time.Minute: 2, 72 * time.Hour: 1.5}
	st := Evaluate(appObjective(), DefaultRules(), windowsAt(burns),
		known(30*24*time.Hour, 1), now)
	if st.State != StateTicket || st.FastBurning || !st.CustomerImpact {
		t.Fatalf("status = %+v, want ticket without a fast burn", st)
	}
	if st.Rules[0].Firing || st.Rules[1].Firing || !st.Rules[2].Firing {
		t.Fatalf("rules = %+v", st.Rules)
	}
}

// The factor is inclusive: exactly 14.4x pages, just under it does not.
func TestEvaluate_FactorBoundary(t *testing.T) {
	quiet := map[time.Duration]float64{6 * time.Hour: 0.5, 30 * time.Minute: 0.5,
		72 * time.Hour: 0.5}
	for _, c := range []struct {
		burn float64
		want State
	}{{14.4, StatePage}, {14.39, StateOK}} {
		burns := map[time.Duration]float64{time.Hour: c.burn, 5 * time.Minute: c.burn}
		for d, b := range quiet {
			burns[d] = b
		}
		st := Evaluate(appObjective(), DefaultRules(), windowsAt(burns),
			known(30*24*time.Hour, 0.5), now)
		if st.State != c.want {
			t.Errorf("burn %v: state %s, want %s", c.burn, st.State, c.want)
		}
	}
}

func TestEvaluate_AllQuietIsOK(t *testing.T) {
	st := Evaluate(appObjective(), DefaultRules(), windowsAt(allBurning(0.2)),
		known(30*24*time.Hour, 0.2), now)
	if st.State != StateOK || st.FastBurning || st.CustomerImpact ||
		len(st.Unknown) != 0 {
		t.Fatalf("status = %+v", st)
	}
	if st.BudgetRemaining == nil || math.Abs(*st.BudgetRemaining-0.8) > 1e-9 {
		t.Fatalf("budget remaining = %v, want 0.8", st.BudgetRemaining)
	}
}

// One unknown window makes its rule unknown; with nothing firing the
// SLO is unknown (with the reason), never ok.
func TestEvaluate_UnknownIsNeverOK(t *testing.T) {
	for _, reason := range []string{ReasonLowTraffic, ReasonZeroEligible, ReasonNoData,
		ReasonStale, ReasonPartialWindow, ReasonSourceError} {
		ws := windowsAt(allBurning(0.1))
		w := ws[5*time.Minute]
		w.Unknown = reason
		ws[5*time.Minute] = w
		st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
		if st.State != StateUnknown || st.FastBurning {
			t.Errorf("%s: state %s", reason, st.State)
		}
		if strings.Join(st.Unknown, ",") != reason || st.Rules[0].Unknown != reason ||
			st.Rules[0].Firing || st.Rules[0].Short.BurnRate != nil {
			t.Errorf("%s: unknown = %v, rule = %+v", reason, st.Unknown, st.Rules[0])
		}
	}
}

// A firing rule wins over an unknown one: the burn is observed.
func TestEvaluate_FiringOverridesUnknown(t *testing.T) {
	ws := windowsAt(allBurning(16))
	w := ws[72*time.Hour]
	w.Unknown = ReasonPartialWindow
	ws[72*time.Hour] = w
	st := Evaluate(appObjective(), DefaultRules(), ws, Window{Duration: 30 * 24 * time.Hour,
		Unknown: ReasonPartialWindow}, now)
	if st.State != StatePage || !st.FastBurning || st.Rules[2].Unknown == "" {
		t.Fatalf("status = %+v", st)
	}
	if st.BudgetRemaining != nil || st.BudgetUnknown != ReasonPartialWindow {
		t.Fatalf("budget = %v (%s), want unknown", st.BudgetRemaining, st.BudgetUnknown)
	}
}

// A window the caller did not evaluate is unknown ("not_evaluated").
func TestEvaluate_MissingWindow(t *testing.T) {
	ws := windowsAt(allBurning(0.1))
	delete(ws, 30*time.Minute)
	st := Evaluate(appObjective(), DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	if st.State != StateUnknown || st.Rules[1].Unknown != ReasonNotEvaluated {
		t.Fatalf("status = %+v", st)
	}
}

// Only a registered app SLI claims customer impact; a proxy page is a
// database proxy burning, not customer impact.
func TestEvaluate_ProxyNeverClaimsCustomerImpact(t *testing.T) {
	o := appObjective()
	o.Kind, o.Source, o.Proxy = KindProxy, SourceProxy, "db_latency"
	st := Evaluate(o, DefaultRules(), windowsAt(allBurning(20)), known(30*24*time.Hour, 3), now)
	if st.State != StatePage || st.CustomerImpact {
		t.Fatalf("proxy status = %+v", st)
	}
	if st.BudgetRemaining == nil || *st.BudgetRemaining >= 0 {
		t.Fatalf("overspent budget = %v, want negative", st.BudgetRemaining)
	}
}

// Configured rules replace the defaults.
func TestEvaluate_CustomRules(t *testing.T) {
	rules := []Rule{{SeverityPage, 2 * time.Hour, 10 * time.Minute, 10}}
	ws := windowsAt(map[time.Duration]float64{2 * time.Hour: 11, 10 * time.Minute: 11})
	st := Evaluate(appObjective(), rules, ws, known(30*24*time.Hour, 1), now)
	if st.State != StatePage || len(st.Rules) != 1 || st.Rules[0].Long.Window != "2h" ||
		st.Rules[0].Short.Window != "10m" {
		t.Fatalf("status = %+v", st)
	}
}

func TestFormatWindow(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", time.Hour: "1h",
		6 * time.Hour: "6h", 72 * time.Hour: "3d", 30 * 24 * time.Hour: "30d",
		90 * time.Second: "90s", 0: "0s"} {
		if got := FormatWindow(d); got != want {
			t.Errorf("FormatWindow(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestObjective_Validate(t *testing.T) {
	prom := appObjective()
	prom.Source = SourcePrometheus
	prom.BadQuery = `sum(increase(http_requests_total{code=~"5.."}[$window])) or vector(0)`
	prom.EligibleQuery = `sum(increase(http_requests_total[$window]))`
	if err := prom.Validate(); err != nil {
		t.Fatalf("prometheus objective: %v", err)
	}
	if err := appObjective().Validate(); err != nil {
		t.Fatalf("push objective: %v", err)
	}
	cases := map[string]func(o *Objective){
		"empty name":        func(o *Objective) { o.Name = "" },
		"upper name":        func(o *Objective) { o.Name = "Checkout" },
		"target 1":          func(o *Objective) { o.Target = 1 },
		"target 0":          func(o *Objective) { o.Target = 0 },
		"nan target":        func(o *Objective) { o.Target = math.NaN() },
		"short window":      func(o *Objective) { o.Window = 30 * time.Minute },
		"long window":       func(o *Objective) { o.Window = 91 * 24 * time.Hour },
		"zero min":          func(o *Objective) { o.MinEligible = 0 },
		"negative stale":    func(o *Objective) { o.StaleAfter = -time.Second },
		"bad kind":          func(o *Objective) { o.Kind = "service" },
		"app from proxy":    func(o *Objective) { o.Source = SourceProxy },
		"push with query":   func(o *Objective) { o.BadQuery = "up" },
		"prom no window":    func(o *Objective) { *o = prom; o.BadQuery = "sum(errors)" },
		"prom no eligible":  func(o *Objective) { *o = prom; o.EligibleQuery = "" },
		"proxy from push":   func(o *Objective) { o.Kind = KindProxy },
		"unknown source":    func(o *Objective) { o.Source = "statsd" },
		"long description":  func(o *Objective) { o.Description = strings.Repeat("d", 301) },
		"control in owner":  func(o *Objective) { o.Owner = "team\nx" },
		"prom resets no wd": func(o *Objective) { *o = prom; o.ResetsQuery = "sum(resets(x[5m]))" },
	}
	for name, mutate := range cases {
		o := appObjective()
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, o)
		}
	}
}
