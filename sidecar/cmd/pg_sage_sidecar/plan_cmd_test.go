package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// plan-03a: the plan CLI against a fake sidecar: arguments, requests,
// output and exit codes (0 decided, 1 refused, 2 usage, 3 auth). The real
// router is in plan_cmd_db_test.go. No concurrency test: one command is
// one sequence of requests with no shared state.

const cliHash = "4f1c2b0d9e8a7f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e"

// fakeSidecar answers each "METHOD path" with a status and body and
// records the requests it got.
type fakeSidecar struct {
	mu      sync.Mutex
	answers map[string]struct {
		status int
		body   string
	}
	got []recordedRequest
}

type recordedRequest struct {
	method, path, query, authz string
	body                       map[string]any
}

func (f *fakeSidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		authz: r.Header.Get("Authorization")}
	_ = json.Unmarshal(raw, &rec.body)
	f.mu.Lock()
	f.got = append(f.got, rec)
	ans, ok := f.answers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		ans.status, ans.body = 404, `{"error":"no such route","code":"not_found"}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ans.status)
	_, _ = io.WriteString(w, ans.body)
}

func (f *fakeSidecar) answer(route string, status int, body string) {
	if f.answers == nil {
		f.answers = map[string]struct {
			status int
			body   string
		}{}
	}
	f.answers[route] = struct {
		status int
		body   string
	}{status, body}
}

func runPlan(t *testing.T, url, token string, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{"SAGE_API_URL": url, "SAGE_API_TOKEN": token}
	var out, errOut bytes.Buffer
	code := runPlanCommand(context.Background(), args, func(k string) string { return env[k] },
		nil, &out, &errOut)
	return code, out.String(), errOut.String()
}

const detailBody = `{"database":"orders","plan_hash":"` + cliHash + `","plan":{"id":7,
	"goal":"cut blocks","status":"approved","steps":[
	{"id":70,"ordinal":1,"proposed_sql":"CREATE INDEX CONCURRENTLY idx_a ON t (a)",
	 "runtime":{"state":"kept"}},
	{"id":72,"ordinal":2,"proposed_sql":"CREATE INDEX CONCURRENTLY idx_b ON t (b)",
	 "runtime":{"state":"awaiting_approval"}}]},
	"coverage":[{"step_id":70,"ordinal":1,"covered":true,"by":"plan"},
	 {"step_id":72,"ordinal":2,"covered":false,"reason":"the plan was approved up to step 1"}]}`

func TestPlanCLI_ApproveSendsTheDecisionWithTheToken(t *testing.T) {
	f := &fakeSidecar{}
	f.answer("POST /api/v1/plans/7/approve", 200, `{"ok":true,"database":"orders",
		"plan_id":7,"approval":{"id":41,"mode":"up_to","up_to":2}}`)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runPlan(t, srv.URL, "pgs_mcp_secret", "approve", "7", "--hash",
		cliHash, "--up-to", "2", "--database", "orders")
	if code != planExitOK || !strings.Contains(out, "approved (up_to 2, approval 41)") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	got := f.got[0]
	if got.authz != "Bearer pgs_mcp_secret" || got.query != "database=orders" ||
		got.body["mode"] != "up_to" || got.body["up_to"] != float64(2) ||
		got.body["via"] != "cli" || got.body["plan_hash"] != cliHash {
		t.Fatalf("request %+v", got)
	}
	if strings.Contains(out+errOut, "pgs_mcp_secret") {
		t.Fatal("the token was printed")
	}
}

// --step N finds step N's id in the plan's detail, then approves it.
func TestPlanCLI_StepApprovalResolvesTheOrdinal(t *testing.T) {
	f := &fakeSidecar{}
	f.answer("GET /api/v1/plans/7", 200, detailBody)
	f.answer("POST /api/v1/plans/7/steps/72/approve", 200, `{"ok":true,"database":"orders"}`)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runPlan(t, srv.URL, "tok", "approve", "7", "--hash", cliHash,
		"--step", "2")
	if code != planExitOK || !strings.Contains(out, "step 2 approved") || len(f.got) != 2 ||
		f.got[1].body["plan_hash"] != cliHash || f.got[1].body["via"] != "cli" {
		t.Fatalf("exit %d, out %q, err %q, requests %+v", code, out, errOut, f.got)
	}
	code, _, errOut = runPlan(t, srv.URL, "tok", "approve", "7", "--hash", cliHash,
		"--step", "9")
	if code != planExitRefused || !strings.Contains(errOut, "has no step 9") {
		t.Fatalf("a missing step: exit %d, err %q", code, errOut)
	}
}

func TestPlanCLI_ShowListAndJSON(t *testing.T) {
	f := &fakeSidecar{}
	f.answer("GET /api/v1/plans/7", 200, detailBody)
	f.answer("GET /api/v1/plans", 200, `{"plans":[{"id":7,"database":"orders",
		"status":"proposed","plan_hash":"`+cliHash+`","goal":"cut blocks"}],"total":1,
		"errors":[]}`)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, _ := runPlan(t, srv.URL, "tok", "show", "7")
	if code != 0 || !strings.Contains(out, "2. [awaiting_approval]") ||
		!strings.Contains(out, "needs approval: the plan was approved up to step 1") {
		t.Fatalf("show: %d %q", code, out)
	}
	code, out, _ = runPlan(t, srv.URL, "tok", "list", "--status", "proposed")
	if code != 0 || !strings.Contains(out, "4f1c2b0d9e8a") || !strings.Contains(out, "orders") ||
		f.got[len(f.got)-1].query != "status=proposed" {
		t.Fatalf("list: %d %q", code, out)
	}
	code, out, _ = runPlan(t, srv.URL, "tok", "show", "7", "--json")
	var parsed map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &parsed) != nil || parsed["plan_hash"] != cliHash {
		t.Fatalf("show --json: %d %q", code, out)
	}
}

// Error propagation: the sidecar's refusal is printed with its code; a
// 409 or 500 exits 1, 401 and 403 exit 3, an unreachable sidecar 1.
func TestPlanCLI_RefusalsPrintTheServerMessage(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   int
		want   string
	}{
		{409, `{"error":"the plan changed after it was shown; review it again",` +
			`"code":"content_changed"}`, planExitRefused, "content_changed"},
		{409, `{"error":"only a halted plan can be resumed","code":"not_halted"}`,
			planExitRefused, "only a halted plan"},
		{500, `{"error":"internal server error"}`, planExitRefused, "internal server error"},
		{401, `{"error":"invalid, expired or revoked API token"}`, planExitAuth, "revoked"},
		{403, `{"error":"the token lacks the approve scope","code":"scope_missing"}`,
			planExitAuth, "scope_missing"},
	}
	for _, c := range cases {
		f := &fakeSidecar{}
		f.answer("POST /api/v1/plans/7/resume", c.status, c.body)
		srv := httptest.NewServer(f)
		code, _, errOut := runPlan(t, srv.URL, "tok", "resume", "7", "--hash", cliHash)
		srv.Close()
		if code != c.code || !strings.Contains(errOut, c.want) {
			t.Errorf("HTTP %d: exit %d, err %q; want %d with %q", c.status, code, errOut,
				c.code, c.want)
		}
	}
	srv := httptest.NewServer(&fakeSidecar{})
	url := srv.URL
	srv.Close()
	if code, _, errOut := runPlan(t, url, "tok", "cancel", "7"); code != planExitRefused ||
		!strings.Contains(errOut, "cannot reach the sidecar") {
		t.Fatalf("closed sidecar: exit %d, err %q", code, errOut)
	}
}

// Invalid input and nil/empty: usage errors exit 2 before any request; a
// missing token exits 3; plain http to another host is refused.
func TestPlanCLI_UsageAndEnvironment(t *testing.T) {
	f := &fakeSidecar{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	usage := [][]string{{}, {"explode"}, {"approve"}, {"approve", "7", "--hash", cliHash},
		{"approve", "7", "--hash", cliHash, "--all", "--up-to", "2"},
		{"approve", "7", "--all"}, {"approve", "x", "--hash", cliHash, "--all"},
		{"approve", "--all", "7"}, {"approve", "7", "--hash", cliHash, "--up-to", "-1"},
		{"reject", "7"}, {"resume", "7"}, {"show", "7", "extra"}, {"show", "0"},
		{"list", "--hash", cliHash}}
	for _, args := range usage {
		if code, _, errOut := runPlan(t, srv.URL, "tok", args...); code != planExitUsage ||
			!strings.Contains(errOut, "usage") {
			t.Errorf("%v: exit %d, err %q, want usage (2)", args, code, errOut)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("usage errors sent %d requests", len(f.got))
	}
	if code, _, _ := runPlan(t, srv.URL, "", "list"); code != planExitAuth {
		t.Errorf("no token: exit %d, want 3", code)
	}
	for _, url := range []string{"", "ftp://x", "http://sage.example.com:8080", "not a url"} {
		if code, _, _ := runPlan(t, url, "tok", "list"); code != planExitUsage {
			t.Errorf("SAGE_API_URL %q: exit %d, want 2", url, code)
		}
	}
	if _, err := planAPIURL("http://localhost:8080"); err != nil {
		t.Errorf("http to localhost refused: %v", err)
	}
	if _, err := planAPIURL("https://sage.example.com/base/"); err != nil {
		t.Errorf("https refused: %v", err)
	}
}
