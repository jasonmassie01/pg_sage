package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Operator control routes (AI-SRE-SPEC §9, CHECK-24/25/26): POST
// /databases/{db}/investigations starts (or coalesces into) an
// investigation of a case; POST .../{id}/stop and .../{id}/resume pause
// and resume it under an If-Match version. Operators only; viewers get
// 403. Errors use canonical codes.

func sreSend(t *testing.T, h http.Handler, path, body string,
	headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

const startBody = `{"case_id":"incident:orders:lock:9","kind":"lock_blocking",` +
	`"subject":"incident 9","idempotency_key":"incident:9"}`

func decodeInvestigation(t *testing.T, body string) sre.Investigation {
	t.Helper()
	var inv sre.Investigation
	if err := json.Unmarshal([]byte(body), &inv); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return inv
}

func TestSREAPI_OperatorStartsAndCoalesces(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	op := sreRouter(t, mgr, testOperatorUser())
	path := "/api/v1/databases/orders/investigations"
	code, body := sreSend(t, op, path, startBody, nil)
	if code != http.StatusCreated {
		t.Fatalf("start = %d %s, want 201", code, body)
	}
	inv := decodeInvestigation(t, body)
	if inv.State != sre.StateQueued || inv.CaseID != "incident:orders:lock:9" ||
		inv.TriggerKind != sre.TriggerLock {
		t.Fatalf("started %+v", inv)
	}
	code, body = sreSend(t, op, path, startBody, nil)
	if code != http.StatusOK || decodeInvestigation(t, body).ID != inv.ID {
		t.Fatalf("repeat start = %d %s, want 200 with the same id", code, body)
	}
	_, events, _ := sreCall(t, op, "GET", path+"/"+string(inv.ID)+"/events")
	if !strings.Contains(events, `"actor":"user:2"`) {
		t.Fatalf("start not attributed to the operator: %s", events)
	}
}

func TestSREAPI_StartRejectsBadRequests(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	op := sreRouter(t, mgr, testOperatorUser())
	path := "/api/v1/databases/orders/investigations"
	for name, body := range map[string]string{
		"not json":      `{`,
		"unknown kind":  `{"case_id":"c","kind":"cosmic_rays"}`,
		"no case":       `{"kind":"lock_blocking"}`,
		"unknown field": `{"case_id":"c","kind":"lock_blocking","run_sql":"DROP TABLE x"}`,
		"too long":      `{"case_id":"` + strings.Repeat("c", 300) + `","kind":"lock_blocking"}`,
	} {
		code, resp := sreSend(t, op, path, body, nil)
		if code != http.StatusBadRequest || !strings.Contains(resp, "invalid_request") {
			t.Errorf("%s: %d %s, want 400 invalid_request", name, code, resp)
		}
	}
	if code, _ := sreSend(t, op, "/api/v1/databases/nope/investigations", startBody,
		nil); code != http.StatusNotFound {
		t.Errorf("unknown database: %d, want 404", code)
	}
}

func TestSREAPI_StopAndResumeWithVersions(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	op := sreRouter(t, mgr, testOperatorUser())
	path := "/api/v1/databases/orders/investigations"
	_, body := sreSend(t, op, path, startBody, nil)
	inv := decodeInvestigation(t, body)
	base := path + "/" + string(inv.ID)
	if code, resp := sreSend(t, op, base+"/stop", `{}`, nil); code != 428 ||
		!strings.Contains(resp, "invalid_request") {
		t.Fatalf("stop without a version = %d %s, want 428", code, resp)
	}
	code, resp := sreSend(t, op, base+"/stop", "", map[string]string{
		"If-Match": fmt.Sprint(inv.Version + 9)})
	if code != http.StatusConflict || !strings.Contains(resp, "version_conflict") {
		t.Fatalf("stale stop = %d %s, want 409 version_conflict", code, resp)
	}
	code, resp = sreSend(t, op, base+"/stop", fmt.Sprintf(`{"version":%d}`, inv.Version),
		nil)
	stopped := decodeInvestigation(t, resp)
	if code != http.StatusOK || stopped.State != sre.StatePaused {
		t.Fatalf("stop = %d %s", code, resp)
	}
	code, resp = sreSend(t, op, base+"/resume", "", map[string]string{
		"If-Match": fmt.Sprintf(`"%d"`, stopped.Version)})
	if code != http.StatusOK || decodeInvestigation(t, resp).State != sre.StateQueued {
		t.Fatalf("resume = %d %s", code, resp)
	}
	code, resp = sreSend(t, op, base+"/resume", fmt.Sprintf(`{"version":%d}`,
		stopped.Version+1), nil)
	if code != http.StatusConflict || !strings.Contains(resp, "invalid_transition") {
		t.Fatalf("resuming a queued investigation = %d %s, want 409 invalid_transition",
			code, resp)
	}
	if code, _ := sreSend(t, op, base+"/stop", `{"version":"x"}`, nil); code != 400 {
		t.Fatalf("a non-numeric version = %d, want 400", code)
	}
}

// CHECK-25: viewers cannot start, stop or resume.
func TestSREAPI_ViewerCannotStartStopOrResume(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	viewer := sreRouter(t, mgr, testViewerUser())
	base := "/api/v1/databases/orders/investigations"
	for _, path := range []string{base, base + "/" + string(orders.ID) + "/stop",
		base + "/" + string(orders.ID) + "/resume"} {
		if code, _ := sreSend(t, viewer, path, startBody, nil); code != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d, want 403", path, code)
		}
	}
	if code, _ := sreSend(t, sreRouter(t, mgr, nil), base, startBody,
		nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated start = %d, want 401", code)
	}
}

// CHECK-26: the same id under another database is not found.
func TestSREAPI_StopIsScopedToTheNamedDatabase(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	op := sreRouter(t, mgr, testOperatorUser())
	_, body := sreSend(t, op, "/api/v1/databases/orders/investigations", startBody, nil)
	inv := decodeInvestigation(t, body)
	code, resp := sreSend(t, op, "/api/v1/databases/billing/investigations/"+
		string(inv.ID)+"/stop", fmt.Sprintf(`{"version":%d}`, inv.Version), nil)
	if code != http.StatusNotFound || !strings.Contains(resp, "not_found") {
		t.Fatalf("cross-database stop = %d %s, want 404", code, resp)
	}
}
