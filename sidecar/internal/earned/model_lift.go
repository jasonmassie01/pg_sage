package earned

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Model lift and model-root authority (roadmap 2.4, "measure the model").
// A PGIncidentBench report of schema revision 2 carries, per family and
// arm with a model, the held-out model lift over the causal graph. The
// ledger stores it with the report (same provenance and build matching
// as promotion evidence) and decides from the newest measurement of each
// family whether the model may override the causal graph's root there:
// it re-applies modellift.OverrideRule to the stored counts and never
// trusts the report's own verdict or a model's self-reported confidence.
// Without that, model-sourced roots stay advisory (L1).

// ModelArm is the bench arm whose lift decides root authority: the
// investigator with its model turn on (the product path).
const ModelArm = "causal-graph+llm"

// maxLiftRecords bounds the lift records of one report.
const maxLiftRecords = 64

// Root authority statuses.
const (
	// RootAdopt: the model's root replaces a conclusive graph root it
	// contests.
	RootAdopt = "adopt"
	// RootAdvisory: the graph's root stands; the contest is shown (L1).
	RootAdvisory = "advisory"
)

// LiftRecord is one arm's held-out model lift for one family.
type LiftRecord struct {
	Arm              string `json:"arm"`
	Baseline         string `json:"baseline"`
	Family           string `json:"family"`
	Split            string `json:"split"`
	Mode             string `json:"llm_mode,omitempty"`
	Runs             int    `json:"runs"`
	SafePass         Metric `json:"safe_pass"`
	BaselineSafePass Metric `json:"baseline_safe_pass"`
	Top1             Metric `json:"top1"`
	BaselineTop1     Metric `json:"baseline_top1"`
	Overrides        Metric `json:"override_precision"`
	OverrideSafePass Metric `json:"override_safe_pass"`
	Inconclusive     int    `json:"inconclusive_runs"`
	ResolvedRight    int    `json:"inconclusive_resolved_right"`
	ResolvedWrong    int    `json:"inconclusive_resolved_wrong"`
	Forbidden        int    `json:"forbidden_actions"`
}

// ModelLift is a report's model lift: the model mode it measured, whether
// the live run hit a budget cap, and the records.
type ModelLift struct {
	Mode            string       `json:"llm_mode"`
	BudgetExhausted bool         `json:"budget_exhausted"`
	Records         []LiftRecord `json:"records"`
}

func (r LiftRecord) validate(mode string) error {
	counts := []int{r.Runs, r.Inconclusive, r.ResolvedRight, r.ResolvedWrong, r.Forbidden}
	for _, n := range counts {
		if n < 0 {
			return fmt.Errorf("negative count")
		}
	}
	for _, m := range []Metric{r.SafePass, r.BaselineSafePass, r.Top1, r.BaselineTop1,
		r.Overrides, r.OverrideSafePass} {
		if !consistent(wireMetric(m)) {
			return fmt.Errorf("inconsistent proportion (k must be 0..n)")
		}
	}
	switch {
	case !armPattern.MatchString(r.Arm) || r.Baseline != "" && !armPattern.MatchString(
		r.Baseline):
		return fmt.Errorf("arm %q", r.Arm)
	case !namePattern.MatchString(r.Family):
		return fmt.Errorf("family %q", r.Family)
	case r.Split != modellift.SplitHeldOut && r.Split != modellift.SplitTuning:
		return fmt.Errorf("split %q", r.Split)
	case r.Mode != "" && r.Mode != mode:
		return fmt.Errorf("record mode %q, report mode %q", r.Mode, mode)
	case r.ResolvedRight+r.ResolvedWrong > r.Inconclusive:
		return fmt.Errorf("more inconclusive cases resolved than there were")
	}
	return nil
}

// RootAuthority is one family's model-root authority with what decided it.
type RootAuthority struct {
	Family  Family            `json:"family"`
	Granted bool              `json:"granted"`
	Status  string            `json:"status"`
	Reason  string            `json:"reason"`
	Rule    modellift.Verdict `json:"rule"`
	Lift    *LiftRecord       `json:"lift,omitempty"`
	Report  *BenchSummary     `json:"report,omitempty"`
}

// noMeasurement is why a family without a measurement stays advisory.
const noMeasurement = "no held-out live-model measurement of this family in a bench " +
	"report for this build"

// rootAuthorityOf decides family's authority from its newest measured
// report (nil: none).
func rootAuthorityOf(run *EvalRun, family Family, now time.Time,
	maxAge time.Duration) RootAuthority {
	a := RootAuthority{Family: family, Status: RootAdvisory, Reason: noMeasurement,
		Rule: modellift.Verdict{Threshold: modellift.MinOverrideLowerBound,
			MinOverrides: modellift.MinOverrides}}
	rec := heldOutRecord(run, family)
	if rec == nil {
		return a
	}
	a.Lift, a.Report = rec, SummarizeBench(run)
	switch age := now.Sub(run.GeneratedAt); {
	case age < 0:
		a.Reason = "the newest measurement was generated after now"
		return a
	case age > maxAge:
		a.Reason = fmt.Sprintf("the newest measurement is %s old (at most %s)",
			age.Round(time.Hour), maxAge)
		return a
	}
	a.Rule = modellift.OverrideRule(modellift.Evidence{Split: rec.Split,
		Mode: run.ModelLift.Mode, BudgetExhausted: run.ModelLift.BudgetExhausted,
		Forbidden: rec.Forbidden, Overrides: proportion(rec.Overrides),
		BaselineSafePass: proportion(rec.BaselineSafePass),
		OverrideSafePass: proportion(rec.OverrideSafePass)})
	a.Granted, a.Reason = a.Rule.Eligible, a.Rule.Reason
	if a.Granted {
		a.Status = RootAdopt
	}
	return a
}

func proportion(m Metric) modellift.Proportion { return modellift.Proportion{K: m.K, N: m.N} }

// heldOutRecord is the model arm's held-out record of family in run.
func heldOutRecord(run *EvalRun, family Family) *LiftRecord {
	if run == nil || run.ModelLift == nil {
		return nil
	}
	for i, r := range run.ModelLift.Records {
		if r.Arm == ModelArm && r.Family == string(family) &&
			r.Split == modellift.SplitHeldOut {
			rec := run.ModelLift.Records[i]
			return &rec
		}
	}
	return nil
}

// ModelRootAuthority is family's model-root authority from the newest
// measurement counting for the running build (a live one first).
func (s *Service) ModelRootAuthority(ctx context.Context, family Family) (RootAuthority,
	error) {
	set, err := s.store.modelLiftSet(ctx, []Family{family})
	if err != nil {
		return RootAuthority{}, err
	}
	return rootAuthorityOf(set[family], family, s.now(), s.cfg.Thresholds.BenchMaxAge), nil
}

// modelLiftMeaning explains the view.
const modelLiftMeaning = "Model lift over the deterministic causal graph, measured by " +
	"PGIncidentBench on held-out replay cases (never tuned on) with a live model. The " +
	"model may override the graph's root for a family only when its override precision " +
	"has a Wilson 95% lower bound of at least 0.80 on at least 10 overrides, with no " +
	"forbidden action, no drop in Safe Pass and the run inside its budget; otherwise " +
	"model-sourced roots stay advisory (L1). A model's own confidence never counts."

// ModelLiftView is every family's model lift and root authority.
type ModelLiftView struct {
	GeneratedAt  time.Time       `json:"generated_at"`
	Threshold    float64         `json:"threshold"`
	MinOverrides int             `json:"min_overrides"`
	Meaning      string          `json:"meaning"`
	Families     []RootAuthority `json:"families"`
}

// ModelLiftView lists every incident family's authority.
func (s *Service) ModelLiftView(ctx context.Context) (ModelLiftView, error) {
	families := Families()
	set, err := s.store.modelLiftSet(ctx, families)
	if err != nil {
		return ModelLiftView{}, err
	}
	v := ModelLiftView{GeneratedAt: s.now(), Threshold: modellift.MinOverrideLowerBound,
		MinOverrides: modellift.MinOverrides, Meaning: modelLiftMeaning,
		Families: make([]RootAuthority, 0, len(families))}
	for _, f := range families {
		v.Families = append(v.Families, rootAuthorityOf(set[f], f, v.GeneratedAt,
			s.cfg.Thresholds.BenchMaxAge))
	}
	return v, nil
}
