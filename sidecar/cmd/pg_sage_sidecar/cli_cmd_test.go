package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cli-01a: the decision loop CLI (plan status form, approve, apply,
// explain) against a sidecar replaying recorded API responses
// (testdata/cli). The real router is in cli_db_test.go. The status form
// exits 0 nothing needs a person, 2 something does, 1 error, 3 auth, 64
// usage; the other commands 0 decided, 1 refused, 2 usage, 3 auth.

const cliTestToken = "pgs_mcp_cliTestSecretThatMustNeverBePrinted0000000"

// replaySidecar answers "METHOD path" (or "METHOD path?query") with a
// status and a recorded body, and records every request it got.
type replaySidecar struct {
	mu      sync.Mutex
	answers map[string]replayAnswer
	got     []replayRequest
}

type replayAnswer struct {
	status int
	body   string
}

type replayRequest struct {
	method, path, query string
	header              http.Header
	body                map[string]any
}

func newReplay() *replaySidecar { return &replaySidecar{answers: map[string]replayAnswer{}} }

// on answers route with status and the body of testdata/cli/file (a body
// starting with "{" is used as is).
func (f *replaySidecar) on(t *testing.T, route string, status int, file string) {
	t.Helper()
	body := file
	if !strings.HasPrefix(file, "{") {
		raw, err := os.ReadFile(filepath.Join("testdata", "cli", file))
		if err != nil {
			t.Fatalf("recorded response %s: %v", file, err)
		}
		body = string(raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[route] = replayAnswer{status: status, body: body}
}

func (f *replaySidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := replayRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		header: r.Header.Clone()}
	_ = json.Unmarshal(raw, &rec.body)
	f.mu.Lock()
	f.got = append(f.got, rec)
	ans, ok := f.answers[r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery]
	if !ok {
		ans, ok = f.answers[r.Method+" "+r.URL.Path]
	}
	f.mu.Unlock()
	if !ok {
		ans = replayAnswer{404, `{"error":"no such route","code":"not_found"}`}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ans.status)
	_, _ = io.WriteString(w, ans.body)
}

// requests returns the recorded requests of method.
func (f *replaySidecar) requests(method string) []replayRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []replayRequest
	for _, r := range f.got {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// runCLIAt runs `args...` with SAGE_API_URL=url (unset when ""), the test
// token and stdin.
func runCLIAt(t *testing.T, url, stdin string, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{"SAGE_API_TOKEN": cliTestToken}
	if url != "" {
		env["SAGE_API_URL"] = url
	}
	return runCLIEnv(t, env, stdin, args...)
}

func runCLIEnv(t *testing.T, env map[string]string, stdin string,
	args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runCLI(context.Background(), args, cliEnv{
		getenv: func(k string) string { return env[k] }, stdin: strings.NewReader(stdin),
		stdout: &out, stderr: &errOut})
	return code, out.String(), errOut.String()
}

// statusSidecar serves the three status reads from recorded responses.
func statusSidecar(t *testing.T, inbox, plans, actions string) (*replaySidecar, string) {
	t.Helper()
	f := newReplay()
	f.on(t, "GET /api/v1/inbox", 200, inbox)
	f.on(t, "GET /api/v1/plans", 200, plans)
	f.on(t, "GET /api/v1/actions/pending", 200, actions)
	f.on(t, "GET /api/v1/whynot", 200, "whynot_901.json")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// Happy path: two inbox items wait on a person, so the status form lists
// them first, then the plans and changes due, then the held-back change
// with its first why-not link, and exits 2.
func TestCLIStatus_ListsWaitingDueAndHeldBackAndExits2(t *testing.T) {
	f, url := statusSidecar(t, "inbox_two.json", "plans.json", "actions_pending.json")
	code, out, errOut := runCLIAt(t, url, "", "plan")
	if code != statusExitNeedsPerson {
		t.Fatalf("exit %d, want 2; out %q err %q", code, out, errOut)
	}
	order := []string{"Waiting on you (2)", "change_approval", "42", "plan_approval", "7",
		"Due", "plan 8 on orders", "approved", "add the orders index",
		"change 43 on orders", "Thu 19 Nov 02:00 UTC",
		"Held back", "change 44 on orders", "outside the maintenance window",
		"why not: window", "It is outside the maintenance window"}
	at := 0
	for _, want := range order {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order after byte %d in:\n%s", want, at, out)
		}
		at += i + len(want)
	}
	for _, unwanted := range []string{"plan 5", "old work", "change 42 on orders:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output lists %q (not due or not held back):\n%s", unwanted, out)
		}
	}
	whynot := 0
	for _, r := range f.requests(http.MethodGet) {
		if r.path == "/api/v1/whynot" {
			whynot++
			if r.query != "database=orders&finding_id=901" {
				t.Errorf("why-not query %q, want database=orders&finding_id=901", r.query)
			}
		}
		if got := r.header.Get("Authorization"); got != "Bearer "+cliTestToken {
			t.Errorf("%s %s sent Authorization %q", r.method, r.path, got)
		}
	}
	if whynot != 1 || len(f.requests(http.MethodPost)) != 0 {
		t.Fatalf("%d why-not reads, %d writes; want 1 and none", whynot,
			len(f.requests(http.MethodPost)))
	}
}

// Nil/empty: nothing waiting, due or held back is "Nothing needs you."
// and exit 0; --database narrows every read.
func TestCLIStatus_NothingWaitingExits0(t *testing.T) {
	f, url := statusSidecar(t, "inbox_empty.json", "plans_empty.json", "actions_empty.json")
	code, out, errOut := runCLIAt(t, url, "", "plan", "--database", "orders")
	if code != statusExitClear || strings.TrimSpace(out) != "Nothing needs you." {
		t.Fatalf("exit %d, out %q, err %q; want 0 and Nothing needs you.", code, out, errOut)
	}
	for _, r := range f.got {
		if !strings.Contains(r.query, "database=orders") {
			t.Errorf("%s %s?%s does not name the database", r.method, r.path, r.query)
		}
	}
	if len(f.got) != 3 {
		t.Fatalf("%d requests, want the inbox, plans and actions reads", len(f.got))
	}
}

// An inbox that could not read a source is a lower bound: with nothing
// listed the CLI cannot say nothing needs a person, so it exits 1 (an
// error), never 0, naming the source.
func TestCLIStatus_UnreadSourceIsAnErrorNotClear(t *testing.T) {
	_, url := statusSidecar(t, "inbox_lower_bound.json", "plans_empty.json",
		"actions_empty.json")
	code, out, errOut := runCLIAt(t, url, "", "plan")
	if code != statusExitError || !strings.Contains(errOut, "sage.action_queue") ||
		strings.Contains(out, "Nothing needs you.") {
		t.Fatalf("exit %d, out %q, err %q; want 1 naming sage.action_queue", code, out, errOut)
	}
}

// --json is the stable form: waiting, due and held_back lists and
// needs_person, with the same exit code.
func TestCLIStatus_JSONIsStable(t *testing.T) {
	_, url := statusSidecar(t, "inbox_two.json", "plans.json", "actions_pending.json")
	code, out, _ := runCLIAt(t, url, "", "plan", "--json")
	var got struct {
		NeedsPerson bool             `json:"needs_person"`
		Waiting     []map[string]any `json:"waiting"`
		Due         []map[string]any `json:"due"`
		HeldBack    []map[string]any `json:"held_back"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if code != statusExitNeedsPerson || !got.NeedsPerson || len(got.Waiting) != 2 ||
		len(got.Due) != 2 || len(got.HeldBack) != 1 {
		t.Fatalf("exit %d, %+v; want 2, needs_person, 2 waiting, 2 due, 1 held back", code, got)
	}
	if got.HeldBack[0]["why_not"] != "window" || got.Due[0]["kind"] != "plan" ||
		got.Due[1]["kind"] != "change" {
		t.Fatalf("held back %v, due %v", got.HeldBack, got.Due)
	}
}

// Invalid input: an unknown flag or a flag without its value on the status
// form exits 64 (2 means a person is needed), before any request.
func TestCLIStatus_UsageErrorsExit64(t *testing.T) {
	f, url := statusSidecar(t, "inbox_two.json", "plans.json", "actions_pending.json")
	for _, args := range [][]string{{"plan", "--bogus"}, {"plan", "--database"},
		{"plan", "--json", "extra"}} {
		code, _, errOut := runCLIAt(t, url, "", args...)
		if code != statusExitUsage || !strings.Contains(errOut, "usage") {
			t.Errorf("%v: exit %d, err %q; want 64 with usage", args, code, errOut)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("usage errors sent %d requests", len(f.got))
	}
}

// Error propagation on the status form: 401 exits 3 with the rejected
// token message; an unreachable sidecar exits 1 naming its host.
func TestCLIStatus_AuthAndTransportErrors(t *testing.T) {
	f := newReplay()
	f.on(t, "GET /api/v1/inbox", 401, `{"error":"invalid, expired or revoked API token"}`)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, _, errOut := runCLIAt(t, srv.URL, "", "plan")
	if code != statusExitAuth || !strings.Contains(errOut, "token rejected (expired or revoked)") {
		t.Fatalf("401: exit %d, err %q; want 3 token rejected", code, errOut)
	}
	closed := httptest.NewServer(newReplay())
	host := closed.Listener.Addr().String()
	closed.Close()
	code, _, errOut = runCLIAt(t, "http://"+host, "", "plan")
	if code != statusExitError || !strings.Contains(errOut, host) {
		t.Fatalf("refused connection: exit %d, err %q; want 1 naming %s", code, errOut, host)
	}
}

// The verbs of plan-03a are unchanged: `plan list` still lists plans.
func TestCLIStatus_PlanVerbsStillRoute(t *testing.T) {
	_, url := statusSidecar(t, "inbox_two.json", "plans.json", "actions_pending.json")
	code, out, errOut := runCLIAt(t, url, "", "plan", "list")
	if code != planExitOK || !strings.Contains(out, "cut blocks") ||
		strings.Contains(out, "Waiting on you") {
		t.Fatalf("plan list: exit %d, out %q, err %q", code, out, errOut)
	}
}

// Nil/empty environment: no SAGE_API_URL and no --url says how to set it
// and exits 1; --url stands in for it; no token exits 3.
func TestCLI_MissingURLAndToken(t *testing.T) {
	for _, args := range [][]string{{"plan"}, {"approve", "42", "--yes"}, {"apply", "42"},
		{"explain", "--database", "orders"}} {
		code, _, errOut := runCLIAt(t, "", "", args...)
		if code != 1 || !strings.Contains(errOut, "set SAGE_API_URL or --url") {
			t.Errorf("%v without a URL: exit %d, err %q; want 1", args, code, errOut)
		}
	}
	_, url := statusSidecar(t, "inbox_empty.json", "plans_empty.json", "actions_empty.json")
	if code, out, errOut := runCLIAt(t, "", "", "plan", "--url", url); code != 0 ||
		!strings.Contains(out, "Nothing needs you.") {
		t.Fatalf("--url: exit %d, out %q, err %q", code, out, errOut)
	}
	code, _, errOut := runCLIEnv(t, map[string]string{"SAGE_API_URL": url}, "", "plan")
	if code != statusExitAuth || !strings.Contains(errOut, "SAGE_API_TOKEN") {
		t.Fatalf("no token: exit %d, err %q; want 3 naming SAGE_API_TOKEN", code, errOut)
	}
	code, _, errOut = runCLIAt(t, "http://sage.example.com:8080", "", "plan")
	if code != 1 || !strings.Contains(errOut, "https") {
		t.Fatalf("plain http to another host: exit %d, err %q; want 1", code, errOut)
	}
}

// An unknown subcommand is a usage error.
func TestCLI_UnknownCommand(t *testing.T) {
	code, _, errOut := runCLIAt(t, "http://localhost:1", "", "launch")
	if code != planExitUsage || !strings.Contains(errOut, "usage") {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
}
