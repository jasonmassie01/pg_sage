package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/whynot"
)

// cli-01a: approve, apply and explain against recorded responses. No
// concurrency test here: one run is one sequence of requests with no
// shared state; two concurrent approvals of one change are refused by the
// server's single pending-to-approved transition (cli_db_test.go).

const card42Hash = "5e1d0c7a9b8f6e4d3c2b1a0918273645f5e4d3c2b1a0918273645f5e4d3c2b1a"

func approveSidecar(t *testing.T) (*replaySidecar, string) {
	t.Helper()
	f := newReplay()
	f.on(t, "GET /api/v1/approvals/42", 200, "card_42.json")
	f.on(t, "POST /api/v1/approvals/42/approve", 200, "approve_42.json")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// Happy path: approve prints the card with its hash, then posts exactly
// that hash once, from the cli surface, and reports the run.
func TestCLIApprove_PrintsTheCardThenPostsItsHashOnce(t *testing.T) {
	f, url := approveSidecar(t)
	code, out, errOut := runCLIAt(t, url, "", "approve", "42", "--yes")
	if code != planExitOK {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	hashAt, doneAt := strings.Index(out, card42Hash), strings.Index(out, "approved")
	if hashAt < 0 || doneAt < hashAt || !strings.Contains(out, "ANALYZE public.orders") ||
		!strings.Contains(out, "safe") || !strings.Contains(out, "verified") {
		t.Fatalf("output does not show the card (hash, SQL, tier) before the result:\n%s", out)
	}
	posts := f.requests(http.MethodPost)
	if len(posts) != 1 || posts[0].body["card_hash"] != card42Hash {
		t.Fatalf("posts %+v, want one with the printed hash", posts)
	}
	if got := posts[0].header.Get("X-Sage-Surface"); got != "cli" {
		t.Fatalf("X-Sage-Surface %q, want cli", got)
	}
	if got := posts[0].header.Get("Authorization"); got != "Bearer "+cliTestToken {
		t.Fatalf("Authorization %q", got)
	}
}

// State transition by a person at a terminal: without --yes, typing the
// database name approves; anything else (or nothing) approves nothing.
func TestCLIApprove_InteractiveConfirmationNeedsTheDatabaseName(t *testing.T) {
	f, url := approveSidecar(t)
	for _, stdin := range []string{"", "nope\n", "order\n"} {
		code, out, errOut := runCLIAt(t, url, stdin, "approve", "42")
		if code != planExitRefused || !strings.Contains(errOut, "not confirmed") ||
			!strings.Contains(out, card42Hash) {
			t.Errorf("stdin %q: exit %d, out %q, err %q; want 1 not confirmed", stdin, code,
				out, errOut)
		}
	}
	if n := len(f.requests(http.MethodPost)); n != 0 {
		t.Fatalf("unconfirmed approvals posted %d times", n)
	}
	code, out, errOut := runCLIAt(t, url, "orders\n", "approve", "42", "--database", "orders")
	posts := f.requests(http.MethodPost)
	if code != planExitOK || len(posts) != 1 || !strings.Contains(out, "type the database") ||
		posts[0].query != "database=orders" {
		t.Fatalf("confirmed: exit %d, posts %+v, out %q, err %q", code, posts, out, errOut)
	}
}

// Invalid input: a non-numeric or missing id, an unknown flag, and
// --up-to 0 are usage errors (exit 2) that send nothing.
func TestCLIApprove_UsageErrorsExit2(t *testing.T) {
	f, url := approveSidecar(t)
	for _, args := range [][]string{{"approve"}, {"approve", "x42", "--yes"},
		{"approve", "0", "--yes"}, {"approve", "42", "--bogus"},
		{"approve", "--plan", "7", "--up-to", "0", "--yes"},
		{"approve", "--plan", "7", "--up-to", "-2", "--yes"},
		{"approve", "42", "--up-to", "2", "--yes"}, {"approve", "42", "43", "--yes"},
		{"apply"}, {"apply", "seven"}, {"apply", "7", "--bogus"},
		{"explain"}, {"explain", "--finding", "901"},
		{"explain", "--database", "orders", "--finding", "x"},
		{"explain", "--database", "orders", "--finding", "1", "--object", "public.t"}} {
		code, _, errOut := runCLIAt(t, url, "", args...)
		if code != planExitUsage || !strings.Contains(errOut, "usage") {
			t.Errorf("%v: exit %d, err %q; want 2 with usage", args, code, errOut)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("usage errors sent %d requests", len(f.got))
	}
}

// Error propagation: each refusal has its own message and exit code.
func TestCLIApprove_RefusalsHaveTheirOwnMessages(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   int
		want   string
	}{
		{"expired token", 401, `{"error":"invalid, expired or revoked API token"}`,
			planExitAuth, "token rejected (expired or revoked)"},
		{"missing scope", 403, `{"error":"the token lacks the approve scope this route ` +
			`needs","code":"scope_missing"}`, planExitAuth, "approve scope"},
		{"agent token", 403, `{"error":"an agent token can never approve a change",` +
			`"code":"approval_reserved_for_humans"}`, planExitAuth,
			"approval_reserved_for_humans"},
		{"edited", 409, `{"error":"the action changed after this card was shown; review ` +
			`it again","code":"content_changed"}`, planExitRefused,
			"the change was edited after it was shown; run plan again"},
		{"decided", 409, `{"error":"the action is no longer awaiting approval (approved)",` +
			`"code":"not_pending"}`, planExitRefused, "no longer awaiting approval"},
		{"capped", 403, `{"error":"approve this change in pg_sage: ` +
			`#/inbox?kind=change_approval&queue=42 (risk tier high is approved in pg_sage ` +
			`only)","code":"remote_approval_capped"}`, planExitRefused,
			"#/inbox?kind=change_approval&queue=42"},
	}
	for _, c := range cases {
		f := newReplay()
		f.on(t, "GET /api/v1/approvals/42", 200, "card_42.json")
		f.on(t, "POST /api/v1/approvals/42/approve", c.status, c.body)
		srv := httptest.NewServer(f)
		code, _, errOut := runCLIAt(t, srv.URL, "", "approve", "42", "--yes")
		srv.Close()
		if code != c.code || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, err %q; want %d with %q", c.name, code, errOut, c.code,
				c.want)
		}
	}
	closed := httptest.NewServer(newReplay())
	host := closed.Listener.Addr().String()
	closed.Close()
	code, _, errOut := runCLIAt(t, "http://"+host, "", "approve", "42", "--yes")
	if code != planExitRefused || !strings.Contains(errOut, host) {
		t.Fatalf("refused connection: exit %d, err %q; want 1 naming %s", code, errOut, host)
	}
}

// approve --plan forwards to the plan approval with the hash it printed:
// all steps, or up to step N.
func TestCLIApprove_PlanForwardsToPlanApprove(t *testing.T) {
	f := newReplay()
	f.on(t, "GET /api/v1/plans/7", 200, detailBody)
	f.on(t, "POST /api/v1/plans/7/approve", 200,
		`{"database":"orders","approval":{"id":3,"mode":"up_to","up_to":1}}`)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runCLIAt(t, srv.URL, "", "approve", "--plan", "7", "--up-to", "1",
		"--yes")
	posts := f.requests(http.MethodPost)
	if code != planExitOK || len(posts) != 1 || !strings.Contains(out, cliHash) {
		t.Fatalf("exit %d, posts %+v, out %q, err %q", code, posts, out, errOut)
	}
	b := posts[0].body
	if b["plan_hash"] != cliHash || b["mode"] != "up_to" || b["up_to"] != float64(1) ||
		b["via"] != "cli" {
		t.Fatalf("plan approve body %v", b)
	}
	if code, _, _ := runCLIAt(t, srv.URL, "", "approve", "--plan", "7", "--yes"); code != 0 {
		t.Fatalf("approve all: exit %d", code)
	}
	if b := f.requests(http.MethodPost)[1].body; b["mode"] != "all" {
		t.Fatalf("approve all body %v", b)
	}
}

// State transitions of apply: a change scheduled outside its window exits
// 1 naming the next window and the why-not window link; inside it, it is
// due and the executor's scheduler runs it (apply posts nothing).
func TestCLIApply_ChangeWaitsForItsWindow(t *testing.T) {
	f := newReplay()
	f.on(t, "GET /api/v1/approvals/43", 200, "card_43_scheduled.json")
	f.on(t, "GET /api/v1/whynot", 200, "whynot_901.json")
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runCLIAt(t, srv.URL, "", "apply", "43")
	all := out + errOut
	if code != planExitRefused || !strings.Contains(all, "Thu 19 Nov 02:00 UTC") ||
		!strings.Contains(all, "It is outside the maintenance window") {
		t.Fatalf("outside the window: exit %d, out %q, err %q", code, out, errOut)
	}
	due := strings.Replace(mustRead(t, "card_43_scheduled.json"), `"due":false`, `"due":true`, 1)
	f.on(t, "GET /api/v1/approvals/43", 200, due)
	code, out, _ = runCLIAt(t, srv.URL, "", "apply", "43")
	if code != planExitOK || !strings.Contains(out, "due") ||
		!strings.Contains(out, "scheduler runs it") {
		t.Fatalf("inside the window: exit %d, out %q", code, out)
	}
	f.on(t, "GET /api/v1/approvals/42", 200, "card_42.json")
	code, _, errOut = runCLIAt(t, srv.URL, "", "apply", "42")
	if code != planExitRefused || !strings.Contains(errOut, "awaiting approval") ||
		!strings.Contains(errOut, "approve 42") {
		t.Fatalf("pending: exit %d, err %q", code, errOut)
	}
	if n := len(f.requests(http.MethodPost)); n != 0 {
		t.Fatalf("apply of a change posted %d requests; it adds no execution route", n)
	}
}

// apply --plan resumes a halted plan with the hash it printed, and only
// reports an approved one (the plan engine runs it step by step).
func TestCLIApply_PlanResumesAHaltedPlan(t *testing.T) {
	f := newReplay()
	f.on(t, "GET /api/v1/plans/9", 200, "plan_9_halted.json")
	f.on(t, "POST /api/v1/plans/9/resume", 200, `{"database":"orders","status":"running"}`)
	f.on(t, "GET /api/v1/plans/7", 200, detailBody)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runCLIAt(t, srv.URL, "", "apply", "--plan", "9", "--yes")
	posts := f.requests(http.MethodPost)
	hash := "9f9f0c7a9b8f6e4d3c2b1a0918273645f5e4d3c2b1a0918273645f5e4d3c2b1a"
	if code != planExitOK || len(posts) != 1 || posts[0].path != "/api/v1/plans/9/resume" ||
		posts[0].body["plan_hash"] != hash {
		t.Fatalf("resume: exit %d, posts %+v, out %q, err %q", code, posts, out, errOut)
	}
	code, out, _ = runCLIAt(t, srv.URL, "", "apply", "--plan", "7")
	if code != planExitOK || !strings.Contains(out, "step by step") ||
		len(f.requests(http.MethodPost)) != 1 {
		t.Fatalf("approved plan: exit %d, out %q", code, out)
	}
	if code, _, errOut := runCLIAt(t, srv.URL, "", "apply", "--plan", "9"); code !=
		planExitRefused || !strings.Contains(errOut, "not confirmed") {
		t.Fatalf("unconfirmed resume: exit %d, err %q", code, errOut)
	}
}

// Happy path: explain prints the ten why-not links in chain order.
func TestCLIExplain_PrintsTheTenLinksInOrder(t *testing.T) {
	f := newReplay()
	f.on(t, "GET /api/v1/whynot", 200, "whynot_901.json")
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runCLIAt(t, srv.URL, "", "explain", "--database", "orders",
		"--finding", "901")
	if code != planExitOK {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
	at := 0
	for i, name := range whynot.Links() {
		j := strings.Index(out[at:], string(name))
		if j < 0 {
			t.Fatalf("link %d %s missing or out of order:\n%s", i+1, name, out)
		}
		at += j
	}
	if !strings.Contains(out, "first failing link: window") ||
		!strings.Contains(out, "wait_until") {
		t.Fatalf("no first failing link or fix:\n%s", out)
	}
	if got := f.requests(http.MethodGet)[0].query; got != "database=orders&finding_id=901" {
		t.Fatalf("query %q", got)
	}
	code, out, _ = runCLIAt(t, srv.URL, "", "explain", "--database", "orders", "--object",
		"public.orders", "--json")
	var parsed map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &parsed) != nil ||
		f.requests(http.MethodGet)[1].query != "database=orders&object=public.orders" {
		t.Fatalf("--object --json: exit %d, out %q, query %q", code, out,
			f.requests(http.MethodGet)[1].query)
	}
}

func mustRead(t *testing.T, file string) string {
	t.Helper()
	f := newReplay()
	f.on(t, "GET /x", 200, file)
	return f.answers["GET /x"].body
}
