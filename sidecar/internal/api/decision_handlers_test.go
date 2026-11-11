package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ledger"
)

// policy-07: GET /api/v1/decisions reads each selected database's open
// withheld decisions. These tests replace the per-database read
// (readDatabaseDecisions); the SQL is covered by the ledger package and
// decision_handlers_db_test.go.

// unreachablePool (router_table_test.go) never connects unless used; the
// read is faked here.

func decisionsFleet(t *testing.T, names ...string) *fleet.DatabaseManager {
	t.Helper()
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, name := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name}, Pool: unreachablePool(t),
			Status: &fleet.InstanceStatus{}})
	}
	return mgr
}

type decisionReads struct {
	mu      sync.Mutex
	filters map[string]ledger.WithheldFilter
}

// fakeDecisionReads serves entries per database name; a database in fail
// returns its error.
func fakeDecisionReads(
	t *testing.T, entries map[string][]ledger.WithheldEntry, fail map[string]error,
) *decisionReads {
	t.Helper()
	reads := &decisionReads{filters: map[string]ledger.WithheldFilter{}}
	previous := readDatabaseDecisions
	readDatabaseDecisions = func(_ context.Context, sel namedPool,
		filter ledger.WithheldFilter) ([]ledger.WithheldEntry, bool, error) {
		reads.mu.Lock()
		reads.filters[sel.name] = filter
		reads.mu.Unlock()
		if err := fail[sel.name]; err != nil {
			return nil, false, err
		}
		list := entries[sel.name]
		if list == nil {
			list = []ledger.WithheldEntry{}
		}
		return list, false, nil
	}
	t.Cleanup(func() { readDatabaseDecisions = previous })
	return reads
}

func decisionsMux(mgr *fleet.DatabaseManager) *http.ServeMux {
	mux := http.NewServeMux()
	registerDecisionRoutes(mux, mgr)
	return mux
}

func budgetEntry(id int64, seen time.Time) ledger.WithheldEntry {
	version := 3
	return ledger.WithheldEntry{DecisionID: id, CandidateKey: "ck_a", Verdict: ledger.VerdictPark,
		Reason: "rate_limit_exceeded", RiskTier: "safe", Feature: "index",
		TargetObjects: []string{"public.orders"}, RepeatCount: 3, OpenRows: 1,
		FirstSeen: seen.Add(-time.Hour), LastSeenAt: seen,
		BudgetDetail: "next frees at 18:00", PolicyVersion: &version,
		PolicySHA256: strings.Repeat("cd", 32)}
}

type decisionsBody struct {
	Database  string              `json:"database"`
	Decisions []map[string]any    `json:"decisions"`
	Partial   []map[string]string `json:"partial"`
	Truncated bool                `json:"truncated"`
	Limit     int                 `json:"limit"`
}

// newAgentTokenRequest is how an MCP agent token would reach a REST
// route: a bearer header and no session user.
func newAgentTokenRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer sage_agent_token_without_operator_scope")
	return req
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeDecisions(t *testing.T, body []byte) decisionsBody {
	t.Helper()
	var out decisionsBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

// Happy path: an operator reads grouped entries with the human reason and
// the budget detail; no SQL text is returned.
func TestDecisionsListsEntriesWithHumanReason(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fakeDecisionReads(t, map[string][]ledger.WithheldEntry{"a": {budgetEntry(7, now)}}, nil)
	rec := requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions?database=a&verdict=parked", "", testOperatorUser())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := decodeDecisions(t, rec.Body.Bytes())
	if len(body.Decisions) != 1 || body.Database != "a" || body.Limit != 100 {
		t.Fatalf("body = %+v, want one decision for a, limit 100", body)
	}
	got := body.Decisions[0]
	if got["database"] != "a" || got["repeat_count"] != float64(3) ||
		got["human_reason"] != "rate limit exceeded (next frees at 18:00)" ||
		got["budget_detail"] != "next frees at 18:00" || got["verdict"] != "parked" ||
		got["policy_version"] != float64(3) ||
		got["policy_doc_sha256"] != strings.Repeat("cd", 32) {
		t.Fatalf("decision = %v", got)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "sql") {
		t.Fatalf("the response carries SQL: %s", rec.Body)
	}
}

// The filter reaches the read as parsed; limit defaults to 100.
func TestDecisionsPassesTheParsedFilter(t *testing.T) {
	reads := fakeDecisionReads(t, nil, nil)
	rec := requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions?database=a&verdict=blocked&reason=outside_maintenance_window"+
			"&since=2026-11-01T18:00:00Z&limit=500", "", testAdminUser())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := reads.filters["a"]
	if got.Verdict != ledger.VerdictBlocked || got.Reason != "outside_maintenance_window" ||
		got.Limit != 500 || got.Since == nil ||
		!got.Since.Equal(time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)) || got.DatabaseID != nil {
		t.Fatalf("filter = %+v", got)
	}
	requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions?database=a", "", testAdminUser())
	if reads.filters["a"].Limit != ledger.DefaultWithheldLimit {
		t.Fatalf("default limit = %d, want 100", reads.filters["a"].Limit)
	}
}

// Invalid input: each bad filter is a 400 naming its field, before any read.
func TestDecisionsRejectsInvalidFilters(t *testing.T) {
	reads := fakeDecisionReads(t, nil, nil)
	mux := decisionsMux(decisionsFleet(t, "a"))
	for query, field := range map[string]string{
		"verdict=foo":      "verdict",
		"verdict=execute":  "verdict",
		"reason=bar":       "reason",
		"limit=-1":         "limit",
		"limit=0":          "limit",
		"limit=501":        "limit",
		"limit=ten":        "limit",
		"since=yesterday":  "since",
		"database=a%27--x": "database",
	} {
		rec := requestPolicy(mux, http.MethodGet, "/api/v1/decisions?"+query, "",
			testOperatorUser())
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("%s: %d %s, want 400 naming %s", query, rec.Code, rec.Body, field)
		}
	}
	if len(reads.filters) != 0 {
		t.Fatalf("an invalid filter reached the database: %v", reads.filters)
	}
}

// Authorization: a viewer gets 403 and no decision data; a request with
// no signed-in user (an agent bearer token is not a session) gets 401.
func TestDecisionsRequiresOperator(t *testing.T) {
	now := time.Now()
	reads := fakeDecisionReads(t,
		map[string][]ledger.WithheldEntry{"a": {budgetEntry(7, now)}}, nil)
	mux := decisionsMux(decisionsFleet(t, "a"))
	for _, path := range []string{"/api/v1/decisions", "/api/v1/policy/explain?finding_id=1"} {
		rec := requestPolicy(mux, http.MethodGet, path, "", testViewerUser())
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "rate_limit") {
			t.Errorf("viewer %s: %d %s, want 403 without data", path, rec.Code, rec.Body)
		}
		req := newAgentTokenRequest(path)
		rec = serve(mux, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("agent token %s: %d, want 401", path, rec.Code)
		}
	}
	if len(reads.filters) != 0 {
		t.Fatalf("a refused request read decisions: %v", reads.filters)
	}
}

// Fleet: database=a reads only a; with every database selected and b
// unreachable, a's rows come back with b named in partial, status 200.
func TestDecisionsFleetPartial(t *testing.T) {
	now := time.Now()
	connErr := &pgconn.ConnectError{}
	reads := fakeDecisionReads(t, map[string][]ledger.WithheldEntry{
		"a": {budgetEntry(7, now)}, "c": {budgetEntry(9, now.Add(time.Minute))},
	}, map[string]error{"b": connErr})
	mux := decisionsMux(decisionsFleet(t, "a", "b", "c"))
	rec := requestPolicy(mux, http.MethodGet, "/api/v1/decisions?database=a", "",
		testOperatorUser())
	body := decodeDecisions(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || len(body.Decisions) != 1 || len(reads.filters) != 1 ||
		body.Decisions[0]["database"] != "a" || len(body.Partial) != 0 {
		t.Fatalf("database=a: %d %+v (reads %v)", rec.Code, body, reads.filters)
	}
	rec = requestPolicy(mux, http.MethodGet, "/api/v1/decisions", "", testOperatorUser())
	body = decodeDecisions(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || body.Database != "all" || len(body.Decisions) != 2 {
		t.Fatalf("all: %d %s", rec.Code, rec.Body)
	}
	if body.Decisions[0]["database"] != "c" || body.Decisions[1]["database"] != "a" {
		t.Fatalf("order = %v, %v; want newest (c) first", body.Decisions[0]["database"],
			body.Decisions[1]["database"])
	}
	if len(body.Partial) != 1 || body.Partial[0]["database"] != "b" ||
		body.Partial[0]["error"] != "connection" {
		t.Fatalf("partial = %v, want b with a connection error", body.Partial)
	}
}

// Error propagation: when every selected database fails, the response is
// 503 and names each one; the raw error text never reaches the client.
func TestDecisionsAllDatabasesFailed(t *testing.T) {
	fakeDecisionReads(t, nil, map[string]error{
		"a": errors.New("dial tcp 10.0.0.9:5432: secret-host refused")})
	rec := requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions", "", testOperatorUser())
	body := decodeDecisions(t, rec.Body.Bytes())
	if rec.Code != http.StatusServiceUnavailable || len(body.Partial) != 1 ||
		body.Partial[0]["database"] != "a" || body.Decisions == nil {
		t.Fatalf("all failed: %d %s, want 503 naming a", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secret-host") {
		t.Fatalf("raw error text reached the client: %s", rec.Body)
	}
}

// Nil/empty: no withheld decisions is [], not null; an unknown database is
// 404; no fleet is an empty list.
func TestDecisionsEmptyAndUnknown(t *testing.T) {
	fakeDecisionReads(t, nil, nil)
	rec := requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions", "", testOperatorUser())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decisions":[]`) ||
		!strings.Contains(rec.Body.String(), `"partial":[]`) {
		t.Fatalf("empty: %d %s, want decisions [] and partial []", rec.Code, rec.Body)
	}
	rec = requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions?database=zz", "", testOperatorUser())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown database: %d, want 404", rec.Code)
	}
	rec = requestPolicy(decisionsMux(nil), http.MethodGet, "/api/v1/decisions", "",
		testOperatorUser())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decisions":[]`) {
		t.Fatalf("no fleet: %d %s", rec.Code, rec.Body)
	}
}

// Boundary: the merged list is cut at limit and says so.
func TestDecisionsMergedLimit(t *testing.T) {
	now := time.Now()
	fakeDecisionReads(t, map[string][]ledger.WithheldEntry{
		"a": {budgetEntry(1, now)}, "b": {budgetEntry(2, now.Add(time.Second))},
	}, nil)
	rec := requestPolicy(decisionsMux(decisionsFleet(t, "a", "b")), http.MethodGet,
		"/api/v1/decisions?limit=1", "", testOperatorUser())
	body := decodeDecisions(t, rec.Body.Bytes())
	if len(body.Decisions) != 1 || !body.Truncated || body.Decisions[0]["database"] != "b" {
		t.Fatalf("limit 1 of 2: %s, want b only and truncated", rec.Body)
	}
}

// Concurrent access: parallel list requests over one fleet.
func TestDecisionsConcurrentRequests(t *testing.T) {
	fakeDecisionReads(t, map[string][]ledger.WithheldEntry{
		"a": {budgetEntry(1, time.Now())}}, nil)
	mux := decisionsMux(decisionsFleet(t, "a", "b"))
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- requestPolicy(mux, http.MethodGet, "/api/v1/decisions", "",
				testOperatorUser()).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent request status %d", code)
		}
	}
}
