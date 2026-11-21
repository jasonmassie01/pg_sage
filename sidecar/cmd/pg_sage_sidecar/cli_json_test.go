package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cli-01a review: with --json, stdout is exactly one JSON document (the
// sidecar's answer) so a script can parse it; the card, the prompt and
// progress text go to stderr. No concurrency test: one run is one
// sequence of requests with no shared state.

// assertOneJSONDocument fails unless out parses as one JSON object.
func assertOneJSONDocument(t *testing.T, what, out string) map[string]any {
	t.Helper()
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s: stdout is not JSON (%v):\n%s", what, err, out)
	}
	if dec.More() {
		t.Fatalf("%s: stdout holds more than one JSON document:\n%s", what, out)
	}
	return doc
}

// Happy path: approve --json prints the card on stderr and only the
// approval answer on stdout.
func TestCLIApprove_JSONStdoutIsOneDocument(t *testing.T) {
	f, url := approveSidecar(t)
	code, out, errOut := runCLIAt(t, url, "", "approve", "42", "--yes", "--json")
	if code != planExitOK {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	doc := assertOneJSONDocument(t, "approve --json", out)
	if doc["queue_id"] != float64(42) || doc["executed"] != true {
		t.Fatalf("stdout is not the approval answer: %v", doc)
	}
	if !strings.Contains(errOut, card42Hash) || strings.Contains(out, card42Hash) {
		t.Fatalf("the card (hash) belongs on stderr under --json:\nout %q\nerr %q", out, errOut)
	}
	if n := len(f.requests(http.MethodPost)); n != 1 {
		t.Fatalf("posts %d, want 1", n)
	}
}

// State transition at a terminal under --json: the typed confirmation's
// prompt is on stderr, and a refusal leaves stdout empty.
func TestCLIApprove_JSONPromptGoesToStderr(t *testing.T) {
	f, url := approveSidecar(t)
	code, out, errOut := runCLIAt(t, url, "nope\n", "approve", "42", "--json")
	if code != planExitRefused || out != "" || !strings.Contains(errOut, "type the database") {
		t.Fatalf("unconfirmed: exit %d, out %q, err %q", code, out, errOut)
	}
	if n := len(f.requests(http.MethodPost)); n != 0 {
		t.Fatalf("an unconfirmed approval posted %d requests", n)
	}
}

// Boundary: the approval names the database of the card it printed even
// when --database was not given (the sidecar resolved the only one).
func TestCLIApprove_PostNamesTheCardsDatabase(t *testing.T) {
	f, url := approveSidecar(t)
	if code, out, errOut := runCLIAt(t, url, "", "approve", "42", "--yes"); code != 0 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	posts := f.requests(http.MethodPost)
	if len(posts) != 1 || posts[0].query != "database=orders" {
		t.Fatalf("posts %+v, want one with database=orders", posts)
	}
}

// Nil/empty: a card without a database is refused before any approval.
func TestCLIApprove_CardWithoutDatabaseIsRefused(t *testing.T) {
	f := newReplay()
	card := strings.Replace(mustRead(t, "card_42.json"), `"database":"orders",`, "", 1)
	f.on(t, "GET /api/v1/approvals/42", 200, card)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, _, errOut := runCLIAt(t, srv.URL, "", "approve", "42", "--yes")
	if code != planExitRefused || !strings.Contains(errOut, "no card hash or database") {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
	if n := len(f.requests(http.MethodPost)); n != 0 {
		t.Fatalf("posted %d requests", n)
	}
}

// State transitions of apply --json: outside the window stdout is the
// card JSON alone (exit 1), and the window text is on stderr.
func TestCLIApply_JSONStdoutIsOneDocument(t *testing.T) {
	f := newReplay()
	f.on(t, "GET /api/v1/approvals/43", 200, "card_43_scheduled.json")
	f.on(t, "GET /api/v1/whynot", 200, "whynot_901.json")
	f.on(t, "GET /api/v1/plans/7", 200, detailBody)
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errOut := runCLIAt(t, srv.URL, "", "apply", "43", "--json")
	doc := assertOneJSONDocument(t, "apply --json", out)
	sched, _ := doc["schedule"].(map[string]any)
	if code != planExitRefused || sched["due"] != false ||
		!strings.Contains(errOut, "Thu 19 Nov 02:00 UTC") {
		t.Fatalf("outside the window: exit %d, doc %v, err %q", code, doc, errOut)
	}
	code, out, errOut = runCLIAt(t, srv.URL, "", "apply", "--plan", "7", "--json")
	doc = assertOneJSONDocument(t, "apply --plan --json", out)
	if code != planExitOK || doc["plan_hash"] == nil || !strings.Contains(errOut, "step by step") {
		t.Fatalf("approved plan: exit %d, doc %v, err %q", code, doc, errOut)
	}
}
