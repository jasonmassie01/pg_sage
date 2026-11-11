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

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/briefing"
)

// llm-06b: GET /api/v1/briefings and /api/v1/briefings/latest over fake
// readers. The real-database round trip is briefing_routes_db_test.go.

type fakeBriefingReader struct {
	mu      sync.Mutex
	records []briefing.Record // newest first
	err     error
	limits  []int
}

func (f *fakeBriefingReader) List(_ context.Context, limit int) ([]briefing.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return nil, f.err
	}
	if limit > len(f.records) {
		limit = len(f.records)
	}
	return append([]briefing.Record{}, f.records[:limit]...), nil
}

func (f *fakeBriefingReader) Latest(ctx context.Context) (briefing.Record, error) {
	list, err := f.List(ctx, 1)
	if err != nil {
		return briefing.Record{}, err
	}
	if len(list) == 0 {
		return briefing.Record{}, briefing.ErrNoBriefing
	}
	return list[0], nil
}

const briefingPayloadJSON = `{"findings":[{"id":7,"category":"index","severity":"critical",` +
	`"title":"Unused index","object_identifier":null,"occurrence_count":2}],` +
	`"total_open":4,"actions":[],"window":{"start":"2026-11-08T06:00:00Z",` +
	`"end":"2026-11-09T06:00:00Z"},"open_by_severity":{"critical":1,"warning":3}}`

func fakeRecord(id int64, at time.Time, payload string) briefing.Record {
	tokens := 120
	return briefing.Record{ID: id, GeneratedAt: at, PeriodStart: at.Add(-24 * time.Hour),
		PeriodEnd: at, Mode: "executive", ContentText: "# Briefing", LLMUsed: true,
		TokenCount: &tokens, ContentJSON: json.RawMessage(payload),
		DeliveryStatus: json.RawMessage(`{"slack":{"status":"sent","at":"2026-11-09T06:00:01Z"}}`)}
}

// fakeFleet maps database names to readers; "" or "all" selects every one.
func fakeFleet(readers map[string]*fakeBriefingReader, order ...string) briefingDeps {
	return briefingDeps{sources: func(database string) ([]briefingSource, bool) {
		if database != "" && database != "all" {
			r, ok := readers[database]
			if !ok {
				return nil, false
			}
			return []briefingSource{{name: database, reader: r}}, true
		}
		out := []briefingSource{}
		for _, name := range order {
			out = append(out, briefingSource{name: name, reader: readers[name]})
		}
		return out, true
	}}
}

func callBriefings(t *testing.T, deps briefingDeps, path string, user *auth.User) (
	int, map[string]any, string) {
	t.Helper()
	mux := http.NewServeMux()
	registerBriefingRoutesWith(mux, deps)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body, w.Body.String()
}

func briefingRows(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["briefings"].([]any)
	if !ok {
		t.Fatalf("briefings is not an array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func oneDB(records ...briefing.Record) briefingDeps {
	return fakeFleet(map[string]*fakeBriefingReader{"prod": {records: records}}, "prod")
}

// Happy path: the latest briefing round-trips content_json unchanged.
func TestBriefingsLatestRoundTripsContentJSON(t *testing.T) {
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	deps := oneDB(fakeRecord(9, at, briefingPayloadJSON), fakeRecord(8, at.Add(-time.Hour), `{}`))
	code, body, raw := callBriefings(t, deps, "/api/v1/briefings/latest", viewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	rows := briefingRows(t, body)
	if len(rows) != 1 || rows[0]["database"] != "prod" || rows[0]["id"] != 9.0 {
		t.Fatalf("latest rows = %v, want prod's briefing 9", rows)
	}
	content, _ := json.Marshal(rows[0]["content_json"])
	assertSameJSON(t, string(content), briefingPayloadJSON)
	delivery, _ := json.Marshal(rows[0]["delivery_status"])
	assertSameJSON(t, string(delivery), `{"slack":{"status":"sent","at":"2026-11-09T06:00:01Z"}}`)
	if rows[0]["generated_at"] != "2026-11-09T06:00:00Z" || rows[0]["llm_used"] != true ||
		rows[0]["token_count"] != 120.0 || rows[0]["content_text"] != "# Briefing" ||
		rows[0]["period_start"] != "2026-11-08T06:00:00Z" || rows[0]["mode"] != "executive" {
		t.Fatalf("latest row fields = %v", rows[0])
	}
}

func assertSameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	_ = json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("JSON changed:\n got %s\nwant %s", gb, wb)
	}
}

// Happy path, fleet: one latest briefing per database that has one.
func TestBriefingsLatestPerDatabase(t *testing.T) {
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	deps := fakeFleet(map[string]*fakeBriefingReader{
		"billing": {records: []briefing.Record{fakeRecord(3, at, `{"total_open":1}`)}},
		"empty":   {},
		"orders":  {records: []briefing.Record{fakeRecord(5, at.Add(time.Hour), `{}`)}},
	}, "billing", "empty", "orders")
	code, body, raw := callBriefings(t, deps, "/api/v1/briefings/latest?database=all",
		viewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	rows := briefingRows(t, body)
	if len(rows) != 2 || rows[0]["database"] != "billing" || rows[1]["database"] != "orders" {
		t.Fatalf("rows = %v, want billing then orders", rows)
	}
	missing, _ := body["without_briefing"].([]any)
	if len(missing) != 1 || missing[0] != "empty" {
		t.Fatalf("without_briefing = %v, want [empty]", body["without_briefing"])
	}
}

// Nil/empty: no briefing yet is a 404 with the reason.
func TestBriefingsLatestNoBriefingYet(t *testing.T) {
	code, body, _ := callBriefings(t, oneDB(), "/api/v1/briefings/latest", viewerUser())
	msg, _ := body["error"].(string)
	if code != http.StatusNotFound || !strings.HasPrefix(msg, "no briefing yet") ||
		!strings.Contains(msg, briefing.NoBriefingReason) {
		t.Fatalf("status %d error %q, want 404 no briefing yet with the reason", code, msg)
	}
	code, body, _ = callBriefings(t, oneDB(), "/api/v1/briefings/latest?database=prod",
		viewerUser())
	msg, _ = body["error"].(string)
	if code != http.StatusNotFound || !strings.Contains(msg, `database "prod"`) ||
		!strings.Contains(msg, briefing.NoBriefingReason) {
		t.Fatalf("named database: status %d error %q", code, msg)
	}
}

// Nil/empty: no connected database is a 503, not "no briefing yet".
func TestBriefingsNoConnectedDatabase(t *testing.T) {
	deps := fakeFleet(map[string]*fakeBriefingReader{})
	for _, path := range []string{"/api/v1/briefings", "/api/v1/briefings/latest"} {
		code, body, _ := callBriefings(t, deps, path, viewerUser())
		msg, _ := body["error"].(string)
		if code != http.StatusServiceUnavailable ||
			!strings.Contains(msg, "no connected database") {
			t.Fatalf("%s: status %d error %q, want 503 no connected database", path, code, msg)
		}
	}
	code, _, _ := callBriefings(t, briefingDeps{}, "/api/v1/briefings", viewerUser())
	if code != http.StatusServiceUnavailable {
		t.Fatalf("zero deps: status %d, want 503", code)
	}
}

// Scope: viewers may read; anyone signed in may; nobody else.
func TestBriefingsReadableByEverySignedInRole(t *testing.T) {
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	deps := oneDB(fakeRecord(1, at, `{}`))
	for _, path := range []string{"/api/v1/briefings", "/api/v1/briefings/latest"} {
		for _, user := range []*auth.User{viewerUser(), testOperatorUser(), testAdminUser()} {
			if code, _, raw := callBriefings(t, deps, path, user); code != http.StatusOK {
				t.Fatalf("%s as %s: status %d (%s)", path, user.Role, code, raw)
			}
		}
		if code, _, _ := callBriefings(t, deps, path, nil); code != http.StatusUnauthorized {
			t.Fatalf("%s anonymous: status %d, want 401", path, code)
		}
	}
}

// Invalid input: a bad database name, an unknown database, a bad limit.
func TestBriefingsInvalidInput(t *testing.T) {
	deps := oneDB()
	cases := map[string]int{
		"/api/v1/briefings?database=bad%20name":        http.StatusBadRequest,
		"/api/v1/briefings/latest?database=bad%3Bname": http.StatusBadRequest,
		"/api/v1/briefings?database=nope":              http.StatusNotFound,
		"/api/v1/briefings/latest?database=nope":       http.StatusNotFound,
		"/api/v1/briefings?limit=0":                    http.StatusBadRequest,
		"/api/v1/briefings?limit=101":                  http.StatusBadRequest,
		"/api/v1/briefings?limit=ten":                  http.StatusBadRequest,
		"/api/v1/briefings?limit=-3":                   http.StatusBadRequest,
		"/api/v1/briefings?limit=1%3B+DROP+TABLE+x":    http.StatusBadRequest,
		"/api/v1/briefings?limit=100&database=prod":    http.StatusOK,
		"/api/v1/briefings?limit=1":                    http.StatusOK,
		"/api/v1/briefings/latest?database=prod&x=bad": http.StatusNotFound,
	}
	for path, want := range cases {
		code, body, _ := callBriefings(t, deps, path, viewerUser())
		if code != want {
			t.Errorf("%s: status %d (%v), want %d", path, code, body, want)
		}
	}
	_, body, _ := callBriefings(t, deps, "/api/v1/briefings?limit=0", viewerUser())
	if msg, _ := body["error"].(string); !strings.Contains(msg, "1 to 100") {
		t.Fatalf("limit error %q does not name the range", msg)
	}
}

// Boundary: the list is newest first across databases and stops at limit;
// each database is asked for at most limit rows.
func TestBriefingsListMergesNewestFirst(t *testing.T) {
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	a := &fakeBriefingReader{records: []briefing.Record{
		fakeRecord(3, at, `{}`), fakeRecord(2, at.Add(-48*time.Hour), `{}`)}}
	b := &fakeBriefingReader{records: []briefing.Record{
		fakeRecord(9, at.Add(-24*time.Hour), `{}`), fakeRecord(8, at.Add(-72*time.Hour), `{}`)}}
	deps := fakeFleet(map[string]*fakeBriefingReader{"a": a, "b": b}, "a", "b")
	code, body, raw := callBriefings(t, deps, "/api/v1/briefings?limit=3", viewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	rows := briefingRows(t, body)
	got := []string{}
	for _, r := range rows {
		got = append(got, r["database"].(string)+":"+jsonNumber(r["id"]))
	}
	if strings.Join(got, ",") != "a:3,b:9,a:2" {
		t.Fatalf("order = %v, want a:3,b:9,a:2", got)
	}
	if len(a.limits) != 1 || a.limits[0] != 3 || b.limits[0] != 3 {
		t.Fatalf("per-database limits a=%v b=%v, want 3", a.limits, b.limits)
	}
	_, body, _ = callBriefings(t, deps, "/api/v1/briefings", viewerUser())
	if rows := briefingRows(t, body); len(rows) != 4 || a.limits[1] != briefing.DefaultListLimit {
		t.Fatalf("default list = %d rows, limit %v", len(rows), a.limits)
	}
}

func jsonNumber(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Nil/empty: a database with no briefing lists as an empty array.
func TestBriefingsListEmptyIsAnArray(t *testing.T) {
	code, body, raw := callBriefings(t, oneDB(), "/api/v1/briefings", viewerUser())
	if code != http.StatusOK || !strings.Contains(raw, `"briefings":[]`) {
		t.Fatalf("status %d body %s, want 200 with an empty array", code, raw)
	}
	if len(briefingRows(t, body)) != 0 {
		t.Fatalf("rows %v", body)
	}
}

// Error propagation: one database's failure is a 500 (or a 503 when its
// pool cannot connect); in a fleet the others still answer and the failed
// database is named.
func TestBriefingsErrorPropagation(t *testing.T) {
	broken := map[string]*fakeBriefingReader{"prod": {err: errors.New("relation missing")}}
	code, body, _ := callBriefings(t, fakeFleet(broken, "prod"), "/api/v1/briefings/latest",
		viewerUser())
	if code != http.StatusInternalServerError || body["error"] != "internal error" {
		t.Fatalf("status %d body %v, want 500 internal error", code, body)
	}
	down := map[string]*fakeBriefingReader{"prod": {err: context.DeadlineExceeded}}
	code, body, _ = callBriefings(t, fakeFleet(down, "prod"), "/api/v1/briefings", viewerUser())
	if code != http.StatusServiceUnavailable || body["code"] != "db_unavailable" {
		t.Fatalf("status %d body %v, want 503 db_unavailable", code, body)
	}
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	mixed := map[string]*fakeBriefingReader{
		"bad": {err: errors.New("boom")}, "good": {records: []briefing.Record{
			fakeRecord(4, at, `{}`)}}}
	for _, path := range []string{"/api/v1/briefings", "/api/v1/briefings/latest"} {
		code, body, raw := callBriefings(t, fakeFleet(mixed, "bad", "good"), path, viewerUser())
		unavailable, _ := body["unavailable"].([]any)
		if code != http.StatusOK || len(briefingRows(t, body)) != 1 ||
			len(unavailable) != 1 || unavailable[0] != "bad" {
			t.Fatalf("%s: status %d body %s, want good's row and bad unavailable", path, code, raw)
		}
		if strings.Contains(raw, "boom") {
			t.Fatalf("%s leaks the database error: %s", path, raw)
		}
	}
}

// Concurrent access: parallel requests over one shared reader all answer.
func TestBriefingsConcurrentRequests(t *testing.T) {
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	deps := oneDB(fakeRecord(1, at, briefingPayloadJSON))
	mux := http.NewServeMux()
	registerBriefingRoutesWith(mux, deps)
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := withUser(httptest.NewRequest(http.MethodGet, "/api/v1/briefings/latest", nil),
				viewerUser())
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			codes <- w.Code
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
