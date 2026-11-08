package analyzer

import (
	"context"
	"errors"
	"testing"
)

// advisors-02: the config advisor reports the categories its deterministic
// re-check fully read, so an applied or retired finding resolves even
// when the advisor emits nothing in its category.

type reportingAdvisor struct {
	findings  []Finding
	evaluated []string
	err       error
}

func (r *reportingAdvisor) Analyze(context.Context) ([]Finding, error) {
	return r.findings, r.err
}

func (r *reportingAdvisor) LastEvaluatedCategories() []string { return r.evaluated }

func TestAdvisorEvaluatedCategoriesResolve(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}, eval: newCycleEval(),
		advisor: &reportingAdvisor{evaluated: []string{"wal_tuning", "advisor_degraded"}}}
	out := a.runAdvisorAndForecaster(context.Background())
	if len(out) != 0 {
		t.Fatalf("findings = %+v, want none", out)
	}
	res := a.eval.resolvable(out)
	if !res["wal_tuning"] || !res["advisor_degraded"] || res["bloat_remediation"] {
		t.Fatalf("resolvable = %v, want exactly the reported categories", res)
	}
}

// A failed advisor evaluated nothing, whatever it reported before.
func TestAdvisorErrorEvaluatesNothing(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}, eval: newCycleEval(),
		advisor: &reportingAdvisor{err: errors.New("boom"),
			evaluated: []string{"wal_tuning"}}}
	a.runAdvisorAndForecaster(context.Background())
	if res := a.eval.resolvable(nil); res["wal_tuning"] {
		t.Fatalf("resolvable = %v after an advisor error", res)
	}
}
