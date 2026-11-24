package slo

import (
	"sort"
	"time"
)

// WindowView is one window of a rule as surfaces show it: the burn rate
// is null when the window is unknown, with the reason.
type WindowView struct {
	Window   string   `json:"window"`
	BurnRate *float64 `json:"burn_rate"`
	Bad      float64  `json:"bad"`
	Eligible float64  `json:"eligible"`
	Unknown  string   `json:"unknown,omitempty"`
}

// RuleResult is one burn-rate rule evaluated.
type RuleResult struct {
	Severity Severity   `json:"severity"`
	Factor   float64    `json:"factor"`
	Long     WindowView `json:"long"`
	Short    WindowView `json:"short"`
	Firing   bool       `json:"firing"`
	Unknown  string     `json:"unknown,omitempty"`
	// Partial names why the other window is unknown when a known window
	// below the factor already decides the rule is not firing.
	Partial string `json:"partial,omitempty"`
}

// Status is one SLO's error-budget state for one database.
type Status struct {
	Name        string     `json:"name"`
	Database    string     `json:"database"`
	Kind        Kind       `json:"kind"`
	Source      SourceKind `json:"source"`
	Description string     `json:"description,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	Target      float64    `json:"target"`
	Window      string     `json:"window"`
	State       State      `json:"state"`
	// FastBurning is true while a page-level rule fires.
	FastBurning bool `json:"fast_burning"`
	// CustomerImpact is claimed only by a registered app SLI that burns.
	CustomerImpact  bool         `json:"customer_impact"`
	Rules           []RuleResult `json:"rules"`
	Unknown         []string     `json:"unknown"`
	BudgetRemaining *float64     `json:"budget_remaining"`
	BudgetUnknown   string       `json:"budget_unknown,omitempty"`
	EvaluatedAt     time.Time    `json:"evaluated_at"`
	StateSince      time.Time    `json:"state_since"`
	BurnStartedAt   *time.Time   `json:"burn_started_at,omitempty"`
	DefinitionHash  string       `json:"definition_hash"`
	// CoverageUnverified is true for a Prometheus SLI without a coverage
	// query: its windows are taken as fully covered and fresh.
	CoverageUnverified bool `json:"coverage_unverified"`
	// BudgetCoverage is the share of the SLO window the data covers, set
	// only when it was measured from data that was read (never for a
	// Prometheus SLI without a coverage query).
	BudgetCoverage *float64 `json:"budget_coverage,omitempty"`
	// BudgetSpentSoFar is the budget the covered part of a partial window
	// spent: its bad/eligible over (1 - target). It is not the budget
	// remaining, which stays unknown.
	BudgetSpentSoFar *float64 `json:"budget_spent_so_far,omitempty"`
	// OwnerDatabase, for a fleet SLO, is the database whose investigator
	// opens its slo_burn investigations (empty: this database).
	OwnerDatabase string `json:"owner_database,omitempty"`
}

// Burning reports whether any rule fires (page or ticket).
func (s Status) Burning() bool { return s.State == StatePage || s.State == StateTicket }

// Evaluate applies the rules to the evaluated windows. A rule fires when
// both of its windows burn at or above its factor; a rule with an
// unknown window is unknown unless the other window decides it (see
// evaluateRule). A firing page rule pages, else a firing ticket rule
// tickets, else any unknown rule makes the SLO unknown, and only then is
// it ok. budget is the window of the whole SLO period.
func Evaluate(o Objective, rules []Rule, windows map[time.Duration]Window, budget Window,
	now time.Time) Status {
	st := Status{Name: o.Name, Database: o.Database, Kind: o.Kind, Source: o.Source,
		Description: o.Description, Owner: o.Owner, Target: o.Target,
		Window: FormatWindow(o.Window), EvaluatedAt: now, Unknown: []string{},
		DefinitionHash: o.Hash(), OwnerDatabase: o.OwnerDatabase}
	reasons := map[string]bool{}
	page, ticket, unknown := false, false, false
	for _, r := range rules {
		rr := evaluateRule(o, r, windows)
		st.Rules = append(st.Rules, rr)
		switch {
		case rr.Firing && r.Severity == SeverityPage:
			page = true
		case rr.Firing:
			ticket = true
		case rr.Unknown != "":
			unknown = true
			reasons[rr.Unknown] = true
		}
	}
	st.State = StateOK
	switch {
	case page:
		st.State, st.FastBurning = StatePage, true
	case ticket:
		st.State = StateTicket
	case unknown:
		st.State = StateUnknown
	}
	for r := range reasons {
		st.Unknown = append(st.Unknown, r)
	}
	sort.Strings(st.Unknown)
	st.CustomerImpact = o.Kind == KindApp && st.Burning()
	st.CoverageUnverified = o.coverageUnverified()
	applyBudget(&st, o, classify(budget, o))
	return st
}

// applyBudget sets the budget left of a known budget window, else why it
// is unknown. The coverage is shown for a window whose data was read
// (known, or only sparse); a partial window with at least the traffic
// floor of eligible events also says what its covered part spent.
func applyBudget(st *Status, o Objective, budget Window) {
	if b, ok := budget.BurnRate(o.Target); ok {
		left := 1 - b
		st.BudgetRemaining = &left
	} else {
		st.BudgetUnknown = unknownOf(budget)
	}
	if st.CoverageUnverified || (budget.Unknown != "" && !sparseReason(budget.Unknown)) {
		return
	}
	coverage := budget.Coverage
	st.BudgetCoverage = &coverage
	if budget.Unknown != ReasonPartialWindow || budget.Eligible <= 0 ||
		budget.Eligible < o.MinEligible || !(o.Target > 0 && o.Target < 1) {
		return
	}
	spent := (budget.Bad / budget.Eligible) / (1 - o.Target)
	st.BudgetSpentSoFar = &spent
}

// evaluateRule applies one rule as a Kleene AND of its windows: both
// known and at or above the factor fire; a known window below the factor
// decides "not firing" when the other window is only sparse (its reason
// is kept as Partial); otherwise an unknown window leaves the rule
// unknown. Absent, stale, failed or invalid data never decides a rule.
func evaluateRule(o Objective, r Rule, windows map[time.Duration]Window) RuleResult {
	rr := RuleResult{Severity: r.Severity, Factor: r.Factor}
	long, longOK := burnView(o, r.Long, windows)
	short, shortOK := burnView(o, r.Short, windows)
	rr.Long, rr.Short = long, short
	// The factor is inclusive, with a tolerance for float rounding.
	floor := r.Factor * (1 - burnTolerance)
	quiet := func(v WindowView, ok bool) bool { return ok && *v.BurnRate < floor }
	switch {
	case longOK && shortOK:
		rr.Firing = *long.BurnRate >= floor && *short.BurnRate >= floor
	case quiet(long, longOK) && sparseReason(short.Unknown):
		rr.Partial = short.Unknown
	case quiet(short, shortOK) && sparseReason(long.Unknown):
		rr.Partial = long.Unknown
	case !longOK:
		rr.Unknown = long.Unknown
	default:
		rr.Unknown = short.Unknown
	}
	return rr
}

// sparseReason reports whether an unknown window still had current data,
// only too little of it: too few or no eligible events, or samples not
// covering the whole window. Such a window cannot keep a rule undecided
// when the rule's other window is known and below the factor.
func sparseReason(reason string) bool {
	switch reason {
	case ReasonLowTraffic, ReasonZeroEligible, ReasonPartialWindow:
		return true
	}
	return false
}

func burnView(o Objective, d time.Duration, windows map[time.Duration]Window) (WindowView,
	bool) {
	v := WindowView{Window: FormatWindow(d)}
	w, ok := windows[d]
	if !ok {
		v.Unknown = ReasonNotEvaluated
		return v, false
	}
	w = classify(w, o)
	v.Bad, v.Eligible = w.Bad, w.Eligible
	b, ok := w.BurnRate(o.Target)
	if !ok {
		v.Unknown = unknownOf(w)
		return v, false
	}
	v.BurnRate = &b
	return v, true
}

func unknownOf(w Window) string {
	if w.Unknown != "" {
		return w.Unknown
	}
	return ReasonZeroEligible
}
