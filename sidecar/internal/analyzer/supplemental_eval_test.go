package analyzer

import (
	"context"
	"errors"
	"testing"
)

// reportingDetector is a supplemental detector that reports which
// categories its run evaluated (the agent posture monitor does).
type reportingDetector struct {
	findings  []Finding
	evaluated []string
	err       error
}

func (d *reportingDetector) Detect(context.Context) ([]Finding, error) {
	return d.findings, d.err
}

func (d *reportingDetector) LastEvaluatedCategories() []string { return d.evaluated }

type plainDetector struct{ findings []Finding }

func (d plainDetector) Detect(context.Context) ([]Finding, error) { return d.findings, nil }

func TestSupplementalDetectors_EvaluatedCategoriesResolve(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}}
	a.eval = newCycleEval()
	clean := &reportingDetector{evaluated: []string{"agent_posture:AP-01",
		"agent_posture:AP-02"}}
	found := &reportingDetector{evaluated: []string{"agent_posture:AP-03"},
		findings: []Finding{{Category: "agent_posture:AP-03", ObjectIdentifier: "public.t"}}}
	a.WithSupplementalDetector(clean)
	a.WithSupplementalDetector(found)
	a.WithSupplementalDetector(plainDetector{findings: []Finding{{Category: "runaway_query"}}})
	out := a.runSupplementalDetectors(context.Background())
	if len(out) != 2 {
		t.Fatalf("findings = %+v, want AP-03 and the runaway finding", out)
	}
	res := a.eval.resolvable(out)
	for _, c := range []string{"agent_posture:AP-01", "agent_posture:AP-02",
		"agent_posture:AP-03", "runaway_query"} {
		if !res[c] {
			t.Errorf("category %s not resolvable: %v", c, res)
		}
	}
}

// A detector that failed evaluated nothing: its categories stay open even
// if it reports categories from an earlier run.
func TestSupplementalDetectors_FailedDetectorEvaluatesNothing(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}}
	a.eval = newCycleEval()
	a.WithSupplementalDetector(&reportingDetector{err: errors.New("connection lost"),
		evaluated: []string{"agent_posture:AP-01"}})
	if out := a.runSupplementalDetectors(context.Background()); len(out) != 0 {
		t.Fatalf("findings = %+v", out)
	}
	if res := a.eval.resolvable(nil); res["agent_posture:AP-01"] {
		t.Fatal("a failed detector's category became resolvable")
	}
}

// A detector that did not run this cycle reports no categories, so its
// open findings are left as they are.
func TestSupplementalDetectors_SkippedRunEvaluatesNothing(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}}
	a.eval = newCycleEval()
	a.WithSupplementalDetector(&reportingDetector{})
	_ = a.runSupplementalDetectors(context.Background())
	if res := a.eval.resolvable(nil); len(res) != 0 {
		t.Fatalf("resolvable = %v, want none", res)
	}
}

// incompleteDetector emitted findings of categories it read only in part
// (a posture detector that hit its row limit).
type incompleteDetector struct {
	reportingDetector
	incomplete []string
}

func (d *incompleteDetector) LastIncompleteCategories() []string { return d.incomplete }

// A category whose run was incomplete is not resolvable even though it
// emitted findings: an open finding on an object past the cut stays open.
// The detector's other categories still resolve.
func TestSupplementalDetectors_IncompleteCategoryDoesNotResolve(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}}
	a.eval = newCycleEval()
	d := &incompleteDetector{reportingDetector: reportingDetector{
		evaluated: []string{"agent_posture:AP-01"},
		findings:  []Finding{{Category: "agent_posture:AP-03", ObjectIdentifier: "s.t01"}}},
		incomplete: []string{"agent_posture:AP-03"}}
	a.WithSupplementalDetector(d)
	out := a.runSupplementalDetectors(context.Background())
	if len(out) != 1 || out[0].ObjectIdentifier != "s.t01" {
		t.Fatalf("findings = %+v, want the incomplete run's finding kept", out)
	}
	res := a.eval.resolvable(out)
	if res["agent_posture:AP-03"] || !res["agent_posture:AP-01"] {
		t.Fatalf("resolvable = %v, want AP-01 and not AP-03", res)
	}
}
