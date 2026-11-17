// Package report builds the review report a person reads on the Review
// page (review-01b). Until a model pass is recorded for a run (review-03a)
// the report is the templated one: the checks that failed or warned,
// ranked in code. Nothing here reaches SQL or the executor.
package report

import (
	"slices"

	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// PassSkipped is the model pass status of a run without one. review-03a
// adds done and failed; a grade-only run never has a model pass.
const PassSkipped = "skipped"

// Reasons a run has no model pass yet.
const (
	reasonGradeOnly = "grade-only run: the model pass runs only in a review"
	reasonNoRecord  = "no model pass is recorded for this run"
)

// ModelPass is the model pass's status: done by Model, or skipped or
// failed with Reason.
type ModelPass struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Finding is one ranked check that failed or warned.
type Finding struct {
	Rank           int           `json:"rank"`
	Dimension      string        `json:"dimension"`
	DimensionTitle string        `json:"dimension_title"`
	Check          string        `json:"check"`
	Status         rubric.Status `json:"status"`
	Critical       bool          `json:"critical,omitempty"`
	Weight         int           `json:"weight"`
	Reason         string        `json:"reason,omitempty"`
	Evidence       []string      `json:"evidence"`
}

// Report is a graded run's report.
type Report struct {
	RunID     int64     `json:"run_id"`
	ModelPass ModelPass `json:"model_pass"`
	Findings  []Finding `json:"findings"`
}

// FromRun is run's templated report. Findings are the failed checks, then
// the warned ones; within each, critical checks first, then by weight
// (heaviest first), then in rubric order. Passed, unknown and not
// applicable checks are not findings: unknowns show on the dimension.
func FromRun(run rubric.Run) Report {
	out := Report{RunID: run.ID, ModelPass: ModelPass{Status: PassSkipped,
		Reason: reasonNoRecord}, Findings: []Finding{}}
	if run.Kind == rubric.KindGrade {
		out.ModelPass.Reason = reasonGradeOnly
	}
	for _, d := range run.Grade.Dimensions {
		for _, c := range d.Checks {
			if c.Status != rubric.StatusFail && c.Status != rubric.StatusWarn {
				continue
			}
			out.Findings = append(out.Findings, Finding{Dimension: d.Dimension,
				DimensionTitle: d.Title, Check: c.Check, Status: c.Status,
				Critical: c.Critical, Weight: c.Weight, Reason: c.Reason,
				Evidence: append([]string{}, c.Evidence...)})
		}
	}
	slices.SortStableFunc(out.Findings, compareFindings)
	for i := range out.Findings {
		out.Findings[i].Rank = i + 1
	}
	return out
}

// compareFindings orders fail before warn, critical first, heavier first;
// a tie keeps rubric order (the sort is stable).
func compareFindings(a, b Finding) int {
	if a.Status != b.Status {
		if a.Status == rubric.StatusFail {
			return -1
		}
		return 1
	}
	if a.Critical != b.Critical {
		if a.Critical {
			return -1
		}
		return 1
	}
	return b.Weight - a.Weight
}
