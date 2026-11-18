package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// review-01b: the grade route's Review page fields (the running run, the
// templated report and the caller's own read receipt) and its golden
// payload, which web/e2e/mocks/review.ts copies.

type reviewPageBody struct {
	Latest   *rubric.Run     `json:"latest"`
	Previous *rubric.Run     `json:"previous"`
	Delta    *rubric.Changes `json:"delta"`
	Running  *struct {
		ID        int64       `json:"id"`
		Kind      rubric.Kind `json:"kind"`
		StartedAt time.Time   `json:"started_at"`
	} `json:"running"`
	Report   *report.RunReport `json:"report"`
	ViewedAt *time.Time        `json:"viewed_at"`
}

// goldenGradeResults is every V1 check passing on a self-managed server
// (the cloud backup checks not applicable), with the overrides given.
func goldenGradeResults(over map[string]rubric.Status) []rubric.CheckResult {
	cloud := map[string]bool{"recoverability.backup_stale": true,
		"recoverability.backup_retention": true, "recoverability.pitr_window": true}
	var out []rubric.CheckResult
	for _, d := range rubric.V1().Dimensions {
		for _, c := range d.Checks {
			res := rubric.CheckResult{Check: c.ID, Status: rubric.StatusPass,
				Evidence: []string{"rubric:" + c.ID}}
			if cloud[c.ID] {
				res.Status, res.Evidence = rubric.StatusNotApplicable, nil
				res.Reason = "platform postgres has no cloud backup telemetry"
			}
			if s, ok := over[c.ID]; ok {
				res.Status = s
				if s == rubric.StatusUnknown {
					res.Evidence, res.Reason = nil, "the producer has not run yet"
				}
			}
			out = append(out, res)
		}
	}
	return out
}

func goldenRun(id int64, at time.Time, g rubric.Grade) rubric.Run {
	done := at.Add(4 * time.Second)
	return rubric.Run{ID: id, Database: "orders", Kind: rubric.KindGrade,
		Triggers: []string{"daily"}, RubricVersion: rubric.Version, Status: rubric.RunDone,
		StartedAt: at, FinishedAt: &done, Grade: g}
}

// goldenLatest is the latest golden grade's checks that do not pass.
func goldenLatest() map[string]rubric.Status {
	return map[string]rubric.Status{
		"recoverability.restore_drill":      rubric.StatusWarn,
		"workload_stability.temp_spill":     rubric.StatusUnknown,
		"security_access.definer_functions": rubric.StatusFail,
		"lifecycle.extension_drift":         rubric.StatusWarn,
		"schema.unindexed_fk":               rubric.StatusFail,
		"configuration.pending_restart":     rubric.StatusWarn,
	}
}

// goldenGradeRuns are the golden's two runs, newest first. Two checks
// moved since the previous grade: schema.duplicate_index fail -> pass and
// lifecycle.extension_drift fail -> warn.
func goldenGradeRuns() []rubric.Run {
	prevAt := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	before := goldenLatest()
	before["schema.duplicate_index"] = rubric.StatusFail
	before["lifecycle.extension_drift"] = rubric.StatusFail
	prev := rubric.Compute(rubric.V1(), goldenGradeResults(before))
	latest := rubric.Compute(rubric.V1(), goldenGradeResults(goldenLatest()))
	return []rubric.Run{goldenRun(12, prevAt.Add(24*time.Hour), latest),
		goldenRun(11, prevAt, prev)}
}

func TestReviewRoutes_GoldenPayloadOfTheReviewPage(t *testing.T) {
	runs := goldenGradeRuns()
	started := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	running := &rubric.Run{ID: 13, Database: "orders", Kind: rubric.KindGrade,
		Status: rubric.RunRunning, StartedAt: started}
	viewed := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	out := buildReviewGrade(runs, running)
	out.ViewedAt = &viewed
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "testdata/review_grade_golden.json", body)
	var got reviewPageBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Latest == nil || got.Latest.Grade.Overall != "B" ||
		len(got.Latest.Grade.Dimensions) != 8 {
		t.Fatalf("latest = %+v, want B overall with 8 dimensions", got.Latest)
	}
	if got.Delta == nil {
		t.Fatal("delta is null")
	}
	moved := 0
	for _, d := range got.Delta.Dimensions {
		moved += len(d.Checks)
	}
	if got.Delta.FirstRun || moved != 2 {
		t.Fatalf("delta = %+v, want two moved checks", got.Delta)
	}
	if got.Running == nil || got.Running.ID != 13 || !got.Running.StartedAt.Equal(started) {
		t.Fatalf("running = %+v, want run 13", got.Running)
	}
	if got.Report == nil || got.Report.RunID != 12 ||
		got.Report.ModelPass.Status != report.PassSkipped || len(got.Report.Findings) == 0 {
		t.Fatalf("report = %+v, want the templated report of run 12", got.Report)
	}
}

func TestReviewRoutes_NoGradeYetHasNullLatestReportAndDelta(t *testing.T) {
	out := buildReviewGrade(nil, nil)
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"latest":null,"previous":null,"delta":null,"running":null,"report":null,` +
		`"viewed_at":null,"why_now":[]}`
	if string(b) != want {
		t.Fatalf("no grade = %s, want %s", b, want)
	}
}

func TestReviewRoutes_RunningBeforeTheFirstGrade(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	out := buildReviewGrade(nil, &rubric.Run{ID: 1, Kind: rubric.KindGrade,
		Status: rubric.RunRunning, StartedAt: at})
	if out.Latest != nil || out.Report != nil || out.Running == nil || out.Running.ID != 1 ||
		!out.Running.StartedAt.Equal(at) {
		t.Fatalf("first run in progress = %+v, want only running", out)
	}
}

func TestReviewRoutes_ViewedRouteIsADocumentedViewerWrite(t *testing.T) {
	reason := viewerWriteRoutes["POST "+reviewViewedPath]
	if reason == "" {
		t.Fatalf("POST %s is not in viewerWriteRoutes", reviewViewedPath)
	}
	if reviewViewedPath != "/api/v1/databases/{db}/review/runs/{id}/viewed" {
		t.Fatalf("viewed path = %s", reviewViewedPath)
	}
}

func TestReviewRoutesDB_RunningThenDoneSwapsTheBanner(t *testing.T) {
	pool := auditPool(t, "review_page_running")
	s := rubric.NewStore(pool)
	seedGrade(t, s, nil)
	ctx := context.Background()
	id, err := s.Start(ctx, rubric.RunSpec{Database: "orders", Kind: rubric.KindGrade,
		RubricVersion: rubric.Version})
	if err != nil {
		t.Fatal(err)
	}
	h := reviewMux(t, pool)
	got := getReviewPage(t, h)
	if got.Running == nil || got.Running.ID != id || got.Latest == nil ||
		got.Latest.ID == id || got.Report == nil || got.Report.RunID != got.Latest.ID {
		t.Fatalf("while running = %+v, want the banner of run %d over the last grade", got, id)
	}
	g := rubric.Compute(rubric.V1(), nil)
	if err := s.Finish(ctx, id, rubric.RunDone, &g, ""); err != nil {
		t.Fatal(err)
	}
	got = getReviewPage(t, h)
	if got.Running != nil || got.Latest == nil || got.Latest.ID != id ||
		got.Report == nil || got.Report.RunID != id {
		t.Fatalf("after finishing = %+v, want run %d's report and no banner", got, id)
	}
}

func getReviewPage(t *testing.T, h http.Handler) reviewPageBody {
	t.Helper()
	code, body := drillGet(t, h, "/api/v1/databases/orders/review/grade")
	var got reviewPageBody
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("grade = %d %s", code, body)
	}
	return got
}
