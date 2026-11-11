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

// ModelLift is a report's model lift: the model mode and model it
// measured (Model normalized by modellift.NormalizeModel; "" when the
// report names none, which grants nothing), whether the live run hit a
// budget cap, and the records.
type ModelLift struct {
	Mode            string       `json:"llm_mode"`
	Model           string       `json:"llm_model"`
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
	// OverridesNeeded is how many more correct held-out overrides the
	// family's newest measurement needs to clear the rule's count
	// conditions (0: enough; the reason names anything else holding it
	// back). Per family: measurements are never pooled across families.
	OverridesNeeded int `json:"overrides_needed"`
	// MeasuredModel is the model the deciding measurement was taken with
	// and RunningModel the one the investigator runs (both normalized;
	// llm-01): authority holds only when they are equal.
	MeasuredModel string        `json:"measured_model"`
	RunningModel  string        `json:"running_model"`
	Lift          *LiftRecord   `json:"lift,omitempty"`
	Report        *BenchSummary `json:"report,omitempty"`
}

// noMeasurement is why a family without a measurement stays advisory:
// only a signed release or local-run report counts (benches-09).
const noMeasurement = "no held-out live-model measurement of this family in a signed " +
	"release or local-run bench report for this build (an unsigned upload never counts; " +
	"install the release's signed report with pg_sage bench install)"

// rootAuthorityOf decides family's authority from its measured report
// (nil: none) for an investigator running runningModel.
func rootAuthorityOf(run *EvalRun, family Family, now time.Time,
	maxAge time.Duration, runningModel string) RootAuthority {
	a := RootAuthority{Family: family, Status: RootAdvisory, Reason: noMeasurement,
		Rule: modellift.Verdict{Threshold: modellift.MinOverrideLowerBound,
			MinOverrides: modellift.MinOverrides},
		OverridesNeeded: modellift.MoreCorrectOverridesNeeded(0, 0),
		RunningModel:    modellift.NormalizeModel(runningModel)}
	rec := heldOutRecord(run, family)
	if rec == nil {
		return a
	}
	a.Lift, a.Report, a.MeasuredModel = rec, SummarizeBench(run), run.ModelLift.Model
	a.OverridesNeeded = modellift.MoreCorrectOverridesNeeded(rec.Overrides.K, rec.Overrides.N)
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
		Mode: run.ModelLift.Mode, Model: a.MeasuredModel, RunningModel: a.RunningModel,
		BudgetExhausted: run.ModelLift.BudgetExhausted,
		Forbidden:       rec.Forbidden, Overrides: proportion(rec.Overrides),
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

// ModelRootAuthority is family's model-root authority for an
// investigator running runningModel, from the newest measurement counting
// for the running build (one of runningModel first, then a live one). A
// change since the history's last entry is recorded (and told) first: an
// investigation is never granted authority the history does not show,
// and a grant that cannot be recorded is not given. A failed
// notification does not withhold it (the history and the notifier's own
// log keep it).
func (s *Service) ModelRootAuthority(ctx context.Context, family Family,
	runningModel string) (RootAuthority, error) {
	set, err := s.store.modelLiftSet(ctx, []Family{family}, runningModel)
	if err != nil {
		return RootAuthority{}, err
	}
	a := rootAuthorityOf(set[family], family, s.now(), s.cfg.Thresholds.BenchMaxAge,
		runningModel)
	if _, err := s.settleRootAuthority(ctx, a); err != nil {
		return RootAuthority{Family: family, Status: RootAdvisory, Reason: "the trust " +
			"ledger could not record the model-root authority, so it is not given"}, err
	}
	return a, nil
}

// modelLiftMeaning explains the view.
const modelLiftMeaning = "Model lift over the deterministic causal graph, measured by " +
	"PGIncidentBench on held-out replay cases (never tuned on) with a live model. The " +
	"model may override the graph's root for a family only when, measured with the " +
	"model the investigator runs now, its override precision " +
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

// ModelLiftView lists every incident family's authority for the model
// the investigator runs now (WithRunningModel).
func (s *Service) ModelLiftView(ctx context.Context) (ModelLiftView, error) {
	families := Families()
	running := s.runningModel()
	set, err := s.store.modelLiftSet(ctx, families, running)
	if err != nil {
		return ModelLiftView{}, err
	}
	v := ModelLiftView{GeneratedAt: s.now(), Threshold: modellift.MinOverrideLowerBound,
		MinOverrides: modellift.MinOverrides, Meaning: modelLiftMeaning,
		Families: make([]RootAuthority, 0, len(families))}
	for _, f := range families {
		v.Families = append(v.Families, rootAuthorityOf(set[f], f, v.GeneratedAt,
			s.cfg.Thresholds.BenchMaxAge, running))
	}
	return v, nil
}

// WithRunningModel gives the ledger the model its database's investigator
// runs, read at each use (a hot reload applies to the next read). The
// hourly reconcile and the model-lift view decide with it; without one
// (nil) the running model is unknown and every family stays advisory.
func (s *Service) WithRunningModel(model func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelOf = model
}

func (s *Service) runningModel() string {
	s.mu.Lock()
	model := s.modelOf
	s.mu.Unlock()
	if model == nil {
		return ""
	}
	return model()
}
