package specialist

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// The HTTP contract: bearer MCP tokens only, every response (errors
// included) carries contract_version, errors use the published codes.

type fakeAuth map[string]Identity

var errAuthStore = errors.New("token store unreachable")

func (a fakeAuth) Authenticate(_ context.Context, secret string) (Identity, error) {
	if secret == "storage-down" {
		return Identity{}, errAuthStore
	}
	id, ok := a[secret]
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

var testAuth = fakeAuth{"read-token": reader, "propose-token": proposer}

func newTestHandler(t *testing.T, limits Limits) (http.Handler, *harness) {
	t.Helper()
	h := newHarness(t, limits)
	handler := NewHandler(h.svc, testAuth, HandlerOptions{StreamWindow: 2 * time.Second,
		StreamInterval: 10 * time.Millisecond})
	return handler, h
}

func call(t *testing.T, h http.Handler, method, path, token,
	body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader = http.NoBody
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", w.Body.String(), err)
	}
	if e.ContractVersion != ContractVersion || e.Error == "" {
		t.Fatalf("error without contract version or message: %s", w.Body.String())
	}
	if w.Header().Get("X-Sage-Contract-Version") != ContractVersion {
		t.Fatalf("missing contract version header on %d", w.Code)
	}
	return e
}

const base = "/api/v1/specialist"

func TestHTTP_ContractAndSchemaArePublic(t *testing.T) {
	h, _ := newTestHandler(t, DefaultLimits())
	w := call(t, h, "GET", base+"/contract", "", "")
	var info ContractInfo
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil ||
		info.ContractVersion != ContractVersion || info.SchemaURL != base+"/openapi.json" ||
		len(info.SupportedVersions) != 1 {
		t.Fatalf("contract %d %s", w.Code, w.Body.String())
	}
	w = call(t, h, "GET", base+"/openapi.json", "", "")
	if w.Code != 200 || normalizedLF(w.Body.Bytes()) != normalizedLF(OpenAPIDocument()) {
		t.Fatalf("openapi %d", w.Code)
	}
}

func TestHTTP_Authentication(t *testing.T) {
	h, hs := newTestHandler(t, DefaultLimits())
	path := base + "/databases/orders/investigations"
	body := `{"symptom":{"summary":"slow"}}`
	cases := []struct {
		name   string
		header string
		status int
		code   string
	}{{"missing", "", 401, "unauthenticated"},
		{"basic scheme", "Basic Zm9vOmJhcg==", 401, "unauthenticated"},
		{"unknown token", "Bearer nope", 401, "unauthenticated"},
		{"empty bearer", "Bearer ", 401, "unauthenticated"},
		{"storage down", "Bearer storage-down", 503, "unavailable"}}
	for _, c := range cases {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.status || errorBody(t, w).Code != c.code {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		if c.status == 401 && !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: no WWW-Authenticate", c.name)
		}
	}
	// A session cookie lends nothing.
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sage_session", Value: "valid-session"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("cookie authenticated the contract: %d", w.Code)
	}
	if len(hs.orders.started) != 0 {
		t.Fatal("an unauthenticated call opened an investigation")
	}
}

func TestHTTP_OpenPollAndReadTheResult(t *testing.T) {
	h, hs := newTestHandler(t, DefaultLimits())
	w := call(t, h, "POST", base+"/databases/orders/investigations", "read-token",
		`{"symptom":{"summary":"checkout slow"},"family":"lock_blocking"}`)
	var open OpenResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &open) != nil || !open.Created ||
		w.Header().Get("X-Sage-Contract-Version") != ContractVersion {
		t.Fatalf("open %d %s", w.Code, w.Body.String())
	}
	w = call(t, h, "POST", base+"/databases/orders/investigations", "read-token",
		`{"symptom":{"summary":"checkout slow"},"family":"lock_blocking"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"match":"idempotent"`) {
		t.Fatalf("repeat %d %s", w.Code, w.Body.String())
	}
	id := open.Investigation.ID
	w = call(t, h, "GET", open.Links.Status, "read-token", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"phase":"queued"`) {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	w = call(t, h, "GET", open.Links.Result, "read-token", "")
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"outcome":"in_progress"`) {
		t.Fatalf("running result %d %s", w.Code, w.Body.String())
	}
	d := lockDetail()
	d.Investigation.ID = sre.UUID(id)
	hs.orders.put(d)
	w = call(t, h, "GET", open.Links.Result, "read-token", "")
	var r Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil ||
		r.RootCause == nil || r.ContractVersion != ContractVersion {
		t.Fatalf("result %d %s", w.Code, w.Body.String())
	}
}

func TestHTTP_ErrorCodes(t *testing.T) {
	limits := DefaultLimits()
	limits.WritesPerMinute = 1
	h, hs := newTestHandler(t, limits)
	hs.orders.put(lockDetail())
	hs.orders.proposals[inv] = []sreaction.ProposalView{cancelProposal(
		sreaction.ProposalProposed)}
	open := base + "/databases/orders/investigations"
	rem := base + "/databases/orders/investigations/" + string(inv) + "/remediations/" +
		cancelID + "/request"
	cases := []struct {
		name, method, path, token, body string
		status                          int
		code                            string
	}{
		{"read token requests", "POST", rem, "read-token", ``, 403, "scope_required"},
		{"other database", "POST", base + "/databases/billing/investigations", "read-token",
			`{"symptom":{"summary":"x"}}`, 403, "database_not_permitted"},
		{"malformed json", "POST", open, "read-token", `{"symptom":`, 400, "invalid_request"},
		{"unknown field", "POST", open, "read-token", `{"symptom":{"summary":"x"},"sudo":1}`,
			400, "invalid_request"},
		{"forced remediation", "POST", rem, "propose-token", `{"force":true}`, 400,
			"invalid_request"},
		{"oversized", "POST", open, "read-token", `{"symptom":{"summary":"x","description":"` +
			strings.Repeat("z", MaxBodyBytes) + `"}}`, 413, "payload_too_large"},
		{"bad investigation id", "GET", base + "/databases/orders/investigations/xyz",
			"read-token", "", 400, "invalid_request"},
		{"missing investigation", "GET", base +
			"/databases/orders/investigations/77777777-7777-4777-8777-777777777777",
			"read-token", "", 404, "not_found"},
		{"unknown route", "GET", base + "/nope", "read-token", "", 404, "not_found"},
		{"invalid database name", "GET", base + "/databases/a;b/investigations/" +
			string(inv), "read-token", "", 400, "invalid_request"},
	}
	for _, c := range cases {
		w := call(t, h, c.method, c.path, c.token, c.body)
		if w.Code != c.status || errorBody(t, w).Code != c.code {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	// Rate limit: the read token's single write is spent on the first open.
	call(t, h, "POST", open, "read-token", `{"symptom":{"summary":"a"},"family":"wal_retention"}`)
	w := call(t, h, "POST", open, "read-token", `{"symptom":{"summary":"b"}}`)
	e := errorBody(t, w)
	if w.Code != 429 || e.Code != "rate_limited" || e.RetryAfterSeconds < 1 ||
		w.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limited: %d %s", w.Code, w.Body.String())
	}
}

func TestHTTP_RemediationReturnsTheGateVerdict(t *testing.T) {
	h, hs := newTestHandler(t, DefaultLimits())
	hs.orders.put(lockDetail())
	hs.orders.proposals[inv] = []sreaction.ProposalView{cancelProposal(
		sreaction.ProposalProposed)}
	w := call(t, h, "POST", base+"/databases/orders/investigations/"+string(inv)+
		"/remediations/"+cancelID+"/request", "propose-token", `{"reason":"PD Q1"}`)
	var resp RemediationResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil ||
		resp.Verdict != "queued_for_approval" || resp.RequestedBy != proposer.Actor() {
		t.Fatalf("remediation %d %s", w.Code, w.Body.String())
	}
	hs.orders.setState(inv, sre.StateCollecting)
	w = call(t, h, "POST", base+"/databases/orders/investigations/"+string(inv)+
		"/remediations/"+cancelID+"/request", "propose-token", ``)
	if w.Code != 409 || errorBody(t, w).Code != "not_requestable" {
		t.Fatalf("running: %d %s", w.Code, w.Body.String())
	}
}

func readEvents(t *testing.T, body io.Reader) []string {
	t.Helper()
	var events []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, ContractVersion) {
			t.Errorf("event data without contract version: %s", line)
		}
	}
	return events
}

func TestHTTP_StreamEndsWhenTheInvestigationIsTerminal(t *testing.T) {
	h, hs := newTestHandler(t, DefaultLimits())
	d := lockDetail()
	d.Investigation.State = sre.StateCollecting
	hs.orders.put(d)
	srv := httptest.NewServer(h)
	defer srv.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		hs.orders.setState(inv, sre.StateConcluded)
	}()
	req, _ := http.NewRequest("GET", srv.URL+base+"/databases/orders/investigations/"+
		string(inv)+"/stream", nil)
	req.Header.Set("Authorization", "Bearer read-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"),
		"text/event-stream") {
		t.Fatalf("stream %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readEvents(t, resp.Body)
	if len(events) < 3 || events[0] != "status" || events[len(events)-1] != "end" {
		t.Fatalf("events %v: a status per change, then end", events)
	}
	statuses := 0
	for _, e := range events {
		if e == "status" {
			statuses++
		}
	}
	if statuses != 2 {
		t.Fatalf("one status event per change, got %d in %v", statuses, events)
	}
}

func TestHTTP_StreamClosesAtItsWindow(t *testing.T) {
	hs := newHarness(t, DefaultLimits())
	h := NewHandler(hs.svc, testAuth, HandlerOptions{StreamWindow: 150 * time.Millisecond,
		StreamInterval: 10 * time.Millisecond})
	d := lockDetail()
	d.Investigation.State = sre.StateCollecting
	hs.orders.put(d)
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+base+"/databases/orders/investigations/"+
		string(inv)+"/stream", nil)
	req.Header.Set("Authorization", "Bearer read-token")
	start := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if time.Since(start) > 2*time.Second || !strings.Contains(string(raw), "event: end") ||
		!strings.Contains(string(raw), `"reason":"window"`) {
		t.Fatalf("window close: %s", raw)
	}
	// An unauthenticated stream is refused before streaming.
	w := call(t, h, "GET", base+"/databases/orders/investigations/"+string(inv)+"/stream",
		"", "")
	if w.Code != 401 {
		t.Fatalf("unauthenticated stream: %d", w.Code)
	}
}
