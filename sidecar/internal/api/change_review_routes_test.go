package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
)

// Change Review over REST (CHECK-SRF-01, -06). Session auth: every
// signed-in role submits, lists, reads and sees the summary; only
// operators and admins record a DBA verdict. The service is real (real
// Postgres store and runner) with a fake deterministic analyzer whose
// verdict follows markers in the SQL; nothing reaches an LLM.

const crPath = "/api/v1/databases/testdb/change-reviews"

type crAPI struct {
	mux      *http.ServeMux
	svc      *changereview.Service
	analyzer *crfake.Analyzer
}

func newCRAPI(t *testing.T, mutate func(*changereview.Deps)) *crAPI {
	t.Helper()
	pool := crfake.Pool(t)
	a := &crAPI{mux: http.NewServeMux(), analyzer: &crfake.Analyzer{}}
	a.svc = crfake.Start(t, pool, "testdb", a.analyzer, mutate)
	reg := changereview.NewRegistry()
	reg.Set("testdb", a.svc)
	registerChangeReviewRoutes(a.mux, reg)
	return a
}

func crAdmin() *auth.User { return &auth.User{ID: 1, Email: "admin@x", Role: "admin"} }

func (a *crAPI) do(t *testing.T, user *auth.User, method, path, body string) (
	*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// submitBody is a review request of sql with optional extra JSON fields.
func submitBody(sql string, extra string) string {
	raw, _ := json.Marshal(sql)
	body := `{"sql":` + string(raw)
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

func (a *crAPI) submitDone(t *testing.T, user *auth.User, sql, extra string) map[string]any {
	t.Helper()
	if extra != "" {
		extra += ","
	}
	w, out := a.do(t, user, "POST", crPath, submitBody(sql, extra+`"wait_seconds":20`))
	if w.Code != http.StatusOK || out["state"] != "complete" {
		t.Fatalf("submit %q: %d %s", sql, w.Code, w.Body.String())
	}
	return out
}

func TestChangeReviewRoutes_RunningReviewReturns202ForEveryRole(t *testing.T) {
	a := newCRAPI(t, nil)
	a.analyzer.Gate = make(chan struct{})
	defer close(a.analyzer.Gate)
	for i, user := range []*auth.User{viewerUser(), operatorUser(), crAdmin()} {
		sql := fmt.Sprintf("ALTER TABLE orders ADD COLUMN c%d text", i)
		w, out := a.do(t, user, "POST", crPath, submitBody(sql, ""))
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s: got %d %s, want 202", user.Role, w.Code, w.Body.String())
		}
		id, _ := out["id"].(string)
		if len(id) != 36 || out["state"] != "running" || out["created"] != true {
			t.Fatalf("%s: body %v, want a running review id and created", user.Role, out)
		}
		if out["source"] != "api" || out["actor"] != fmt.Sprintf("user:%d", user.ID) {
			t.Fatalf("%s: source/actor %v/%v", user.Role, out["source"], out["actor"])
		}
	}
	if w, _ := a.do(t, nil, "POST", crPath, submitBody("SELECT 1", "")); w.Code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous submit: %d, want 401", w.Code)
	}
}

func TestChangeReviewRoutes_WaitReturns200WithTheCompleteReview(t *testing.T) {
	a := newCRAPI(t, nil)
	sql := "ALTER TABLE orders DROP COLUMN email; " + crfake.MarkRisky
	out := a.submitDone(t, viewerUser(), sql, "")
	if out["verdict"] != "risky" || out["deterministic_verdict"] != "risky" {
		t.Fatalf("verdict %v/%v, want risky", out["verdict"], out["deterministic_verdict"])
	}
	md, _ := out["markdown"].(string)
	marker, _ := out["marker"].(string)
	if !strings.HasPrefix(md, "<!-- pg_sage-review db=testdb id="+out["id"].(string)) ||
		!strings.HasPrefix(md, marker) {
		t.Fatalf("markdown must start with the marker %q, got %.120q", marker, md)
	}
	if _, ok := out["transcript"]; ok {
		t.Fatal("the REST view must not carry the model transcript")
	}
	again := a.submitDone(t, viewerUser(), sql, "")
	if again["id"] != out["id"] || again["created"] != false {
		t.Fatalf("repeat: id %v created %v, want %v and false", again["id"], again["created"],
			out["id"])
	}
	if a.analyzer.Calls() != 1 {
		t.Fatalf("analyzer ran %d times, want 1 (the repeat is deduplicated)",
			a.analyzer.Calls())
	}
}

func TestChangeReviewRoutes_SubmitRejectsInvalidRequests(t *testing.T) {
	a := newCRAPI(t, nil)
	cases := map[string]string{
		"empty sql and diff":  `{"sql":"  "}`,
		"malformed json":      `{"sql":`,
		"unknown field":       `{"sql":"SELECT 1","approve":true}`,
		"other database":      `{"sql":"SELECT 1","database":"other"}`,
		"wait too long":       `{"sql":"SELECT 1","wait_seconds":51}`,
		"unknown tool":        `{"sql":"SELECT 1","migration_tool":"cargo"}`,
		"http pr url":         `{"sql":"SELECT 1","pr":{"url":"http://github.com/o/r/pull/1"}}`,
		"sql over max bytes":  submitBody(strings.Repeat("x", 100001), ""),
		"wrong type for wait": `{"sql":"SELECT 1","wait_seconds":"5"}`,
	}
	for name, body := range cases {
		w, out := a.do(t, viewerUser(), "POST", crPath, body)
		if w.Code != http.StatusBadRequest || out["code"] != "invalid_request" {
			t.Errorf("%s: got %d %v, want 400 invalid_request", name, w.Code, out)
		}
	}
	_, out := a.do(t, viewerUser(), "POST", crPath, submitBody(strings.Repeat("x", 100001),
		""))
	if msg, _ := out["error"].(string); !strings.Contains(msg, "max_sql_bytes") {
		t.Fatalf("over-limit message %q must name change_review.max_sql_bytes", msg)
	}
	if a.analyzer.Calls() != 0 {
		t.Fatalf("an invalid request reached the analyzer %d times", a.analyzer.Calls())
	}
}

func TestChangeReviewRoutes_UnknownDatabaseIs404AndBadNameIs400(t *testing.T) {
	a := newCRAPI(t, nil)
	other := "/api/v1/databases/nope/change-reviews"
	id := "/00000000-0000-0000-0000-000000000001"
	probes := []struct {
		user               *auth.User
		method, path, body string
	}{
		{viewerUser(), "POST", other, submitBody("SELECT 1", "")},
		{viewerUser(), "GET", other, ""},
		{viewerUser(), "GET", other + id, ""},
		{operatorUser(), "POST", other + id + "/verdict", `{"verdict":"concur"}`},
		{viewerUser(), "GET", "/api/v1/change-reviews/summary?database=nope", ""},
	}
	for _, p := range probes {
		if w, out := a.do(t, p.user, p.method, p.path, p.body); w.Code !=
			http.StatusNotFound || out["code"] != "not_found" {
			t.Errorf("%s %s: got %d %v, want 404 not_found", p.method, p.path, w.Code, out)
		}
	}
	for _, path := range []string{"/api/v1/databases/all/change-reviews",
		"/api/v1/databases/bad%20name/change-reviews"} {
		if w, _ := a.do(t, viewerUser(), "GET", path, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d, want 400", path, w.Code)
		}
	}
}

func TestChangeReviewRoutes_ListFiltersByPRStateAndLimit(t *testing.T) {
	a := newCRAPI(t, nil)
	prA, prB := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"
	first := a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN a int",
		`"pr":{"url":"`+prA+`"}`)
	second := a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN b int",
		`"pr":{"url":"`+prA+`"}`)
	a.submitDone(t, viewerUser(), "ALTER TABLE t ADD COLUMN c int", `"pr":{"url":"`+prB+`"}`)
	_, out := a.do(t, viewerUser(), "GET", crPath+"?pr_url="+prA, "")
	reviews := crList(t, out)
	if len(reviews) != 2 || reviews[0]["id"] != second["id"] ||
		reviews[1]["id"] != first["id"] {
		t.Fatalf("pr filter: %v, want the two reviews of %s newest first", ids(reviews), prA)
	}
	if reviews[0]["revision_of"] != first["id"] {
		t.Fatalf("the second revision must point at the first: %v", reviews[0]["revision_of"])
	}
	_, out = a.do(t, viewerUser(), "GET", crPath+"?limit=1&state=complete", "")
	if got := crList(t, out); len(got) != 1 || got[0]["pr_url"] != prB {
		t.Fatalf("limit 1: %v, want the newest (%s)", ids(got), prB)
	}
	_, out = a.do(t, viewerUser(), "GET", crPath+"?state=running", "")
	if got := crList(t, out); len(got) != 0 {
		t.Fatalf("state running: %v, want none", ids(got))
	}
	for _, q := range []string{"?limit=abc", "?limit=101", "?limit=-1", "?state=bogus"} {
		if w, _ := a.do(t, viewerUser(), "GET", crPath+q, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d, want 400", q, w.Code)
		}
	}
}

func crList(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	items, ok := out["reviews"].([]any)
	if !ok {
		t.Fatalf("no reviews array in %v", out)
	}
	list := make([]map[string]any, 0, len(items))
	for _, it := range items {
		list = append(list, it.(map[string]any))
	}
	return list
}

func ids(list []map[string]any) []any {
	out := make([]any, 0, len(list))
	for _, r := range list {
		out = append(out, r["id"])
	}
	return out
}

func TestChangeReviewRoutes_GetReturnsTheReviewOr404(t *testing.T) {
	a := newCRAPI(t, nil)
	done := a.submitDone(t, viewerUser(), "CREATE TABLE x (id bigint primary key)", "")
	w, out := a.do(t, viewerUser(), "GET", crPath+"/"+done["id"].(string), "")
	if w.Code != http.StatusOK || out["id"] != done["id"] || out["verdict"] != "safe" ||
		out["markdown"] != done["markdown"] {
		t.Fatalf("get: %d %v", w.Code, out)
	}
	for _, id := range []string{"00000000-0000-0000-0000-00000000abcd", "not-a-uuid"} {
		if w, out := a.do(t, viewerUser(), "GET", crPath+"/"+id, ""); w.Code !=
			http.StatusNotFound || out["code"] != "not_found" {
			t.Errorf("get %s: %d %v, want 404", id, w.Code, out)
		}
	}
	if w, _ := a.do(t, viewerUser(), "GET", crPath+"/"+done["id"].(string)+
		"?wait_seconds=99", ""); w.Code != http.StatusBadRequest {
		t.Errorf("wait_seconds 99: %d, want 400", w.Code)
	}
}

func TestChangeReviewRoutes_GetWaitsForARunningReview(t *testing.T) {
	a := newCRAPI(t, nil)
	a.analyzer.Gate = make(chan struct{})
	w, out := a.do(t, viewerUser(), "POST", crPath, submitBody("SELECT 1", ""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d", w.Code)
	}
	id := out["id"].(string)
	_, now := a.do(t, viewerUser(), "GET", crPath+"/"+id, "")
	if now["state"] != "running" {
		t.Fatalf("before release: state %v, want running", now["state"])
	}
	go func() { time.Sleep(300 * time.Millisecond); close(a.analyzer.Gate) }()
	w, later := a.do(t, viewerUser(), "GET", crPath+"/"+id+"?wait_seconds=20", "")
	if w.Code != http.StatusOK || later["state"] != "complete" {
		t.Fatalf("after wait: %d state %v, want 200 complete", w.Code, later["state"])
	}
}

func TestChangeReviewRoutes_NilRegistryRegistersNothing(t *testing.T) {
	mux := http.NewServeMux()
	registerChangeReviewRoutes(mux, nil)
	req := withUser(httptest.NewRequest("GET", crPath, nil), viewerUser())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("nil registry: %d, want 404 (no route)", w.Code)
	}
}
