package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
)

// DBA verdicts, the summary and the error mapping of the Change Review
// routes (CHECK-SRF-01, CHECK-SRF-06).

func TestChangeReviewRoutes_VerdictIsForOperatorsAndAdminsOnce(t *testing.T) {
	a := newCRAPI(t, nil)
	review := a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN x int "+
		crfake.MarkReview, "")
	path := crPath + "/" + review["id"].(string) + "/verdict"
	if w, _ := a.do(t, viewerUser(), "POST", path, `{"verdict":"concur"}`); w.Code !=
		http.StatusForbidden {
		t.Fatalf("viewer verdict: %d, want 403", w.Code)
	}
	if w, _ := a.do(t, nil, "POST", path, `{"verdict":"concur"}`); w.Code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous verdict: %d, want 401", w.Code)
	}
	bad := map[string]string{
		"unknown verdict":        `{"verdict":"approve"}`,
		"override without why":   `{"verdict":"override","direction":"too_strict"}`,
		"override without dir":   `{"verdict":"override","reason":"fine at our size"}`,
		"unknown field":          `{"verdict":"concur","actor":"user:999"}`,
		"malformed":              `{"verdict":`,
		"direction on a concur?": `{"verdict":"concur","direction":"sideways"}`,
	}
	for name, body := range bad {
		if w, out := a.do(t, operatorUser(), "POST", path, body); w.Code !=
			http.StatusBadRequest || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v, want 400 invalid_request", name, w.Code, out)
		}
	}
	w, out := a.do(t, operatorUser(), "POST", path, `{"verdict":"concur"}`)
	if w.Code != http.StatusOK || out["dba_verdict"] != "concur" ||
		out["dba_actor"] != "user:2" || out["credit_state"] != "pending" {
		t.Fatalf("operator concur: %d %v", w.Code, out)
	}
	w, out = a.do(t, crAdmin(), "POST", path, `{"verdict":"override","direction":"too_strict",
		"reason":"small table"}`)
	if w.Code != http.StatusConflict || out["code"] != "already_decided" ||
		out["decided_by"] != "user:2" || out["dba_verdict"] != "concur" {
		t.Fatalf("second verdict: %d %v, want 409 naming user:2's concur", w.Code, out)
	}
}

func TestChangeReviewRoutes_AdminOverridesWithReasonAndDirection(t *testing.T) {
	a := newCRAPI(t, nil)
	review := a.submitDone(t, viewerUser(), "ALTER TABLE t DROP COLUMN y "+
		crfake.MarkRisky, "")
	w, out := a.do(t, crAdmin(), "POST", crPath+"/"+review["id"].(string)+"/verdict",
		`{"verdict":"override","direction":"too_strict","reason":"column unused since v3"}`)
	if w.Code != http.StatusOK || out["dba_verdict"] != "override" ||
		out["dba_direction"] != "too_strict" || out["dba_reason"] != "column unused since v3" ||
		out["dba_actor"] != "user:1" || out["credit_state"] != "none" {
		t.Fatalf("admin override: %d %v", w.Code, out)
	}
}

func TestChangeReviewRoutes_VerdictOnARunningReviewIs400(t *testing.T) {
	a := newCRAPI(t, nil)
	a.analyzer.Gate = make(chan struct{})
	defer close(a.analyzer.Gate)
	_, out := a.do(t, viewerUser(), "POST", crPath, submitBody("SELECT 1", ""))
	w, body := a.do(t, operatorUser(), "POST", crPath+"/"+out["id"].(string)+"/verdict",
		`{"verdict":"concur"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body["error"]),
		"running") {
		t.Fatalf("verdict on a running review: %d %v, want 400 naming the state", w.Code, body)
	}
}

func TestChangeReviewRoutes_ConcurrentVerdictsStoreExactlyOne(t *testing.T) {
	a := newCRAPI(t, nil)
	review := a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN z int "+
		crfake.MarkReview, "")
	path := crPath + "/" + review["id"].(string) + "/verdict"
	codes := make(chan int, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, _ := a.do(t, operatorUser(), "POST", path, `{"verdict":"concur"}`)
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if count[http.StatusOK] != 1 || count[http.StatusConflict] != 7 {
		t.Fatalf("8 racing verdicts: %v, want one 200 and seven 409", count)
	}
}

func TestChangeReviewRoutes_DisabledIs503ReviewDisabled(t *testing.T) {
	a := newCRAPI(t, func(d *changereview.Deps) { d.Config.Enabled = false })
	w, out := a.do(t, viewerUser(), "POST", crPath, submitBody("SELECT 1", ""))
	if w.Code != http.StatusServiceUnavailable || out["code"] != "review_disabled" {
		t.Fatalf("disabled: %d %v, want 503 review_disabled", w.Code, out)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "change_review.enabled") {
		t.Fatalf("disabled message %v must name change_review.enabled", out["error"])
	}
}

func TestChangeReviewRoutes_BusyIs429WithRetryAfter(t *testing.T) {
	a := newCRAPI(t, func(d *changereview.Deps) { d.Config.MaxConcurrentPerDatabase = 1 })
	a.analyzer.Gate = make(chan struct{})
	defer close(a.analyzer.Gate)
	for i := 0; i < 1+changereview.QueueLength; i++ {
		sql := fmt.Sprintf("ALTER TABLE t ADD COLUMN q%d int", i)
		if w, _ := a.do(t, viewerUser(), "POST", crPath, submitBody(sql, "")); w.Code !=
			http.StatusAccepted {
			t.Fatalf("submit %d: %d, want 202", i, w.Code)
		}
	}
	w, out := a.do(t, viewerUser(), "POST", crPath, submitBody("SELECT 99", ""))
	if w.Code != http.StatusTooManyRequests || out["code"] != "review_busy" ||
		w.Header().Get("Retry-After") == "" {
		t.Fatalf("10th submit: %d %v Retry-After %q, want 429 review_busy", w.Code, out,
			w.Header().Get("Retry-After"))
	}
}

func TestChangeReviewRoutes_ExhaustedModelBudgetCompletesDeterministically(t *testing.T) {
	a := newCRAPI(t, func(d *changereview.Deps) {
		d.Reasoner = crfake.Reasoner{Status: changereview.ModelBudget}
	})
	out := a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN w int "+
		crfake.MarkReview, "")
	if out["model_status"] != "budget" || out["verdict"] != "review" ||
		out["deterministic_verdict"] != "review" {
		t.Fatalf("budget: model_status %v verdict %v/%v, want budget and the deterministic "+
			"review", out["model_status"], out["verdict"], out["deterministic_verdict"])
	}
}

func TestChangeReviewRoutes_SummaryForOneDatabaseAndTheFleet(t *testing.T) {
	a := newCRAPI(t, nil)
	a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN s1 int", "")
	a.submitDone(t, viewerUser(), "ALTER TABLE t DROP COLUMN s2 "+crfake.MarkRisky, "")
	for _, q := range []string{"", "?database=testdb", "?days=7"} {
		w, out := a.do(t, viewerUser(), "GET", "/api/v1/change-reviews/summary"+q, "")
		if w.Code != http.StatusOK || out["reviews"] != float64(2) {
			t.Fatalf("summary%s: %d %v, want 200 with 2 reviews", q, w.Code, out)
		}
		mix, _ := out["verdict_mix"].(map[string]any)
		if mix["safe"] != float64(1) || mix["risky"] != float64(1) ||
			fmt.Sprint(out["databases"]) != "[testdb]" || out["partial"] != false {
			t.Fatalf("summary%s: %v", q, out)
		}
		auto, _ := out["auto_clear"].(map[string]any)
		if auto["label"] != changereview.AutoClearLabel {
			t.Fatalf("auto-clear label %v, want %q", auto["label"], changereview.AutoClearLabel)
		}
	}
	for _, q := range []string{"?days=0", "?days=366", "?days=x", "?database=bad%20name"} {
		if w, _ := a.do(t, viewerUser(), "GET", "/api/v1/change-reviews/summary"+q, ""); w.Code !=
			http.StatusBadRequest {
			t.Errorf("summary%s: %d, want 400", q, w.Code)
		}
	}
}

func TestChangeReviewError_MapsEveryServiceError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: bad", changereview.ErrInvalid), 400, "invalid_request"},
		{fmt.Errorf("%w: review x", changereview.ErrNotFound), 404, "not_found"},
		{changereview.ErrDisabled, 503, "review_disabled"},
		{fmt.Errorf("%w: full", changereview.ErrBusy), 429, "review_busy"},
		{fmt.Errorf("%w: twice", changereview.ErrAlreadyDecided), 409, "already_decided"},
		{fmt.Errorf("%w: store", changereview.ErrUnavailable), 503, "review_unavailable"},
		{errors.New("connection refused to 10.0.0.5"), 500, "internal_error"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		changeReviewError(w, tc.err)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%v: %d %s, want %d %s", tc.err, w.Code, w.Body.String(), tc.status,
				tc.code)
		}
		if strings.Contains(w.Body.String(), "10.0.0.5") {
			t.Errorf("an internal error leaked its detail: %s", w.Body.String())
		}
	}
}
