package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// review-01b: the templated report of a graded run, the one the Review
// page shows when no model pass ran. No concurrent access tests: FromRun
// is a pure function of its argument.

func check(id string, s rubric.Status, w int, critical bool) rubric.CheckResult {
	return rubric.CheckResult{Check: id, Status: s, Weight: w, Critical: critical,
		Evidence: []string{"finding:" + id}, Reason: "reason of " + id}
}

func gradedRun(kind rubric.Kind) rubric.Run {
	return rubric.Run{ID: 42, Database: "orders", Kind: kind, Status: rubric.RunDone,
		Grade: rubric.Grade{RubricVersion: rubric.Version, Dimensions: []rubric.DimensionGrade{
			{Dimension: "recoverability", Title: "Recoverability", Checks: []rubric.CheckResult{
				check("recoverability.drill", rubric.StatusWarn, 20, false),
				check("recoverability.wal", rubric.StatusFail, 5, true),
				check("recoverability.pass", rubric.StatusPass, 50, false),
			}},
			{Dimension: "schema", Title: "Schema", Checks: []rubric.CheckResult{
				check("schema.unknown", rubric.StatusUnknown, 90, false),
				check("schema.fk_index", rubric.StatusFail, 10, false),
				check("schema.dup", rubric.StatusFail, 10, false),
				check("schema.na", rubric.StatusNotApplicable, 10, false),
			}},
		}}}
}

func TestFromRun_RanksFailsBeforeWarnsCriticalFirstThenWeightThenRubricOrder(t *testing.T) {
	r := FromRun(gradedRun(rubric.KindGrade))
	want := []struct {
		check, dim, title string
		status            rubric.Status
	}{
		{"recoverability.wal", "recoverability", "Recoverability", rubric.StatusFail},
		{"schema.fk_index", "schema", "Schema", rubric.StatusFail},
		{"schema.dup", "schema", "Schema", rubric.StatusFail},
		{"recoverability.drill", "recoverability", "Recoverability", rubric.StatusWarn},
	}
	if r.RunID != 42 || len(r.Findings) != len(want) {
		t.Fatalf("report = %+v, want run 42 with %d findings", r, len(want))
	}
	for i, w := range want {
		f := r.Findings[i]
		if f.Rank != i+1 || f.Check != w.check || f.Dimension != w.dim ||
			f.DimensionTitle != w.title || f.Status != w.status {
			t.Errorf("finding %d = %+v, want rank %d %s", i, f, i+1, w.check)
		}
		if len(f.Evidence) != 1 || f.Evidence[0] != "finding:"+w.check ||
			f.Reason != "reason of "+w.check {
			t.Errorf("finding %d evidence/reason = %v %q", i, f.Evidence, f.Reason)
		}
	}
	if !r.Findings[0].Critical || r.Findings[1].Critical || r.Findings[0].Weight != 5 {
		t.Errorf("critical/weight not carried: %+v", r.Findings[0])
	}
}

func TestFromRun_PassUnknownAndNotApplicableAreNotFindings(t *testing.T) {
	for _, f := range FromRun(gradedRun(rubric.KindGrade)).Findings {
		switch f.Status {
		case rubric.StatusPass, rubric.StatusUnknown, rubric.StatusNotApplicable:
			t.Errorf("finding %s has status %s", f.Check, f.Status)
		}
	}
}

func TestFromRun_ModelPassIsSkippedWithTheReasonOfItsKind(t *testing.T) {
	grade := FromRun(gradedRun(rubric.KindGrade)).ModelPass
	if grade.Status != PassSkipped || !strings.Contains(grade.Reason, "grade-only run") ||
		grade.Model != "" {
		t.Fatalf("grade-only model pass = %+v, want skipped: grade-only run", grade)
	}
	review := FromRun(gradedRun(rubric.KindReview)).ModelPass
	if review.Status != PassSkipped || !strings.Contains(review.Reason, "no model pass") {
		t.Fatalf("review model pass = %+v, want skipped: no model pass recorded", review)
	}
}

func TestFromRun_EmptyGradeHasAnEmptyFindingListNotNull(t *testing.T) {
	r := FromRun(rubric.Run{ID: 1, Kind: rubric.KindGrade})
	if r.Findings == nil || len(r.Findings) != 0 {
		t.Fatalf("findings = %#v, want an empty list", r.Findings)
	}
	b, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"findings":[]`) {
		t.Fatalf("json = %s %v, want findings []", b, err)
	}
}

func TestFromRun_NilEvidenceIsAnEmptyList(t *testing.T) {
	run := rubric.Run{ID: 2, Kind: rubric.KindGrade, Grade: rubric.Grade{
		Dimensions: []rubric.DimensionGrade{{Dimension: "schema", Title: "Schema",
			Checks: []rubric.CheckResult{{Check: "schema.x", Status: rubric.StatusFail,
				Weight: 1}}}}}}
	b, err := json.Marshal(FromRun(run))
	if err != nil || !strings.Contains(string(b), `"evidence":[]`) {
		t.Fatalf("json = %s %v, want evidence []", b, err)
	}
}

func TestFromRun_DoesNotShareTheRunsEvidenceSlices(t *testing.T) {
	run := gradedRun(rubric.KindGrade)
	r := FromRun(run)
	r.Findings[0].Evidence[0] = "changed"
	if run.Grade.Dimensions[0].Checks[1].Evidence[0] != "finding:recoverability.wal" {
		t.Fatal("the report aliases the run's evidence slice")
	}
}
