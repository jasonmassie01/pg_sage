package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
)

// GET /api/v1/change-reviews: the Reviews page's list, for one database
// (?database=name) or merged across every database running Change Review
// (no database, or "all"), newest first. A database whose read fails is
// listed in unavailable and the list is partial, not failed.

const crFleetPath = "/api/v1/change-reviews"

type crFleet struct {
	api        *crAPI
	svc2       *changereview.Service
	pool2      *pgxpool.Pool
	registered *changereview.Registry
}

// newCRFleet serves two databases: testdb (the package database) and db2
// (a database of its own).
func newCRFleet(t *testing.T) crFleet {
	t.Helper()
	a := &crAPI{mux: http.NewServeMux(), analyzer: &crfake.Analyzer{}}
	a.svc = crfake.Start(t, crfake.Pool(t), "testdb", a.analyzer, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool2, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "api_cr_fleet2"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool2.Close)
	if err := schema.Bootstrap(ctx, pool2); err != nil {
		t.Fatalf("bootstrap db2: %v", err)
	}
	svc2 := crfake.Start(t, pool2, "db2", &crfake.Analyzer{}, nil)
	reg := changereview.NewRegistry()
	reg.Set("testdb", a.svc)
	reg.Set("db2", svc2)
	registerChangeReviewRoutes(a.mux, reg)
	return crFleet{api: a, svc2: svc2, pool2: pool2, registered: reg}
}

func reviewIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["reviews"].([]any)
	if !ok {
		t.Fatalf("body %v has no reviews list", body)
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	return out
}

func strList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

func TestChangeReviewFleetList_MergesDatabasesNewestFirst(t *testing.T) {
	f := newCRFleet(t)
	first := submitCR(t, f.api.svc, "ALTER TABLE a ADD COLUMN x text")
	second := submitCR(t, f.svc2, "ALTER TABLE b ADD COLUMN y text")
	third := submitCR(t, f.api.svc, "ALTER TABLE c ADD COLUMN z text "+crfake.MarkRisky)
	for _, q := range []string{"", "?database=all"} {
		w, body := f.api.do(t, viewerUser(), "GET", crFleetPath+q, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET%s: %d %s", q, w.Code, w.Body.String())
		}
		got := reviewIDs(t, body)
		want := []string{third.ID, second.ID, first.ID}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("GET%s ids %v, want newest first %v", q, got, want)
		}
		if dbs := strList(body["databases"]); len(dbs) != 2 || dbs[0] != "db2" ||
			dbs[1] != "testdb" {
			t.Fatalf("databases %v, want [db2 testdb]", dbs)
		}
		if body["partial"] != false || len(strList(body["unavailable"])) != 0 {
			t.Fatalf("partial %v unavailable %v", body["partial"], body["unavailable"])
		}
	}
	reviews := mustReviews(t, f, crFleetPath)
	if reviews[1]["database"] != "db2" || reviews[0]["database"] != "testdb" ||
		reviews[0]["verdict"] != "risky" {
		t.Fatalf("views %v, want each review's own database and verdict", reviews)
	}
}

func mustReviews(t *testing.T, f crFleet, path string) []map[string]any {
	t.Helper()
	_, body := f.api.do(t, viewerUser(), "GET", path, "")
	raw, _ := body["reviews"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestChangeReviewFleetList_LimitAndFilters(t *testing.T) {
	f := newCRFleet(t)
	submitCRTo(t, f.api.svc, "ALTER TABLE a ADD COLUMN x text", 7)
	newest := submitCRTo(t, f.svc2, "ALTER TABLE b ADD COLUMN y text", 7)
	_, body := f.api.do(t, viewerUser(), "GET", crFleetPath+"?limit=1", "")
	if got := reviewIDs(t, body); len(got) != 1 || got[0] != newest.ID {
		t.Fatalf("limit=1: %v, want only the newest review %s", got, newest.ID)
	}
	_, body = f.api.do(t, viewerUser(), "GET", crFleetPath+"?database=db2", "")
	if got := reviewIDs(t, body); len(got) != 1 || got[0] != newest.ID {
		t.Fatalf("database=db2: %v, want db2's review only", got)
	}
	if dbs := strList(body["databases"]); len(dbs) != 1 || dbs[0] != "db2" {
		t.Fatalf("databases %v, want [db2]", dbs)
	}
	_, body = f.api.do(t, viewerUser(), "GET", crFleetPath+"?state=running", "")
	if got := reviewIDs(t, body); len(got) != 0 {
		t.Fatalf("state=running: %v, want none", got)
	}
	_, body = f.api.do(t, viewerUser(), "GET",
		crFleetPath+"?pr_url=https://github.com/acme/app/pull/7&state=complete", "")
	if got := reviewIDs(t, body); len(got) != 2 {
		t.Fatalf("pr_url filter: %v, want both reviews of the PR", got)
	}
}

func TestChangeReviewFleetList_RejectsBadInput(t *testing.T) {
	f := newCRFleet(t)
	tests := []struct {
		query string
		code  int
		err   string
	}{
		{"?database=nope", http.StatusNotFound, "not_found"},
		{"?database=bad'db", http.StatusBadRequest, "invalid_request"},
		{"?limit=abc", http.StatusBadRequest, "invalid_request"},
		{"?limit=-1", http.StatusBadRequest, "invalid_request"},
		{"?limit=101", http.StatusBadRequest, "invalid_request"},
		{"?state=bogus", http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range tests {
		w, body := f.api.do(t, viewerUser(), "GET", crFleetPath+tc.query, "")
		if w.Code != tc.code || body["code"] != tc.err {
			t.Errorf("GET%s: %d %v, want %d %s", tc.query, w.Code, body, tc.code, tc.err)
		}
	}
	if w, _ := f.api.do(t, nil, "GET", crFleetPath, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d, want 401", w.Code)
	}
}

func TestChangeReviewFleetList_ReportsUnavailableDatabases(t *testing.T) {
	f := newCRFleet(t)
	r1 := submitCR(t, f.api.svc, "ALTER TABLE a ADD COLUMN x text")
	f.pool2.Close() // db2's reads now fail
	w, body := f.api.do(t, viewerUser(), "GET", crFleetPath, "")
	if w.Code != http.StatusOK {
		t.Fatalf("a failing database must not fail the list: %d %s", w.Code, w.Body.String())
	}
	if got := reviewIDs(t, body); len(got) != 1 || got[0] != r1.ID {
		t.Fatalf("reviews %v, want testdb's review", got)
	}
	if un := strList(body["unavailable"]); len(un) != 1 || un[0] != "db2" ||
		body["partial"] != true {
		t.Fatalf("unavailable %v partial %v, want [db2] and partial", un, body["partial"])
	}
	// Naming the failing database alone is an error, not an empty list.
	w, body = f.api.do(t, viewerUser(), "GET", crFleetPath+"?database=db2", "")
	if w.Code != http.StatusServiceUnavailable || body["code"] != "review_unavailable" {
		t.Fatalf("database=db2: %d %v, want 503 review_unavailable", w.Code, body)
	}
}

func TestChangeReviewFleetList_EmptyRegistry(t *testing.T) {
	mux := http.NewServeMux()
	registerChangeReviewRoutes(mux, changereview.NewRegistry())
	a := &crAPI{mux: mux}
	w, body := a.do(t, viewerUser(), "GET", crFleetPath, "")
	if w.Code != http.StatusOK || len(reviewIDs(t, body)) != 0 ||
		len(strList(body["databases"])) != 0 {
		t.Fatalf("empty registry: %d %v, want an empty list", w.Code, body)
	}
}

func TestChangeReviewFleetList_NilRegistryOmitsTheRoute(t *testing.T) {
	mux := http.NewServeMux()
	registerChangeReviewRoutes(mux, nil)
	a := &crAPI{mux: mux}
	if w, _ := a.do(t, viewerUser(), "GET", crFleetPath, ""); w.Code != http.StatusNotFound {
		t.Fatalf("nil registry: %d, want 404 (route not registered)", w.Code)
	}
}

func TestMergeReviewLists_OrdersAndCuts(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 1, 0, m, 0, 0, time.UTC) }
	parts := [][]changereview.Review{
		{{ID: "a3", CreatedAt: at(3)}, {ID: "a1", CreatedAt: at(1)}},
		{{ID: "b4", CreatedAt: at(4)}, {ID: "b2", CreatedAt: at(2)}},
		nil,
		{{ID: "c3", CreatedAt: at(3)}},
	}
	got := mergeReviewLists(parts, 4)
	want := []string{"b4", "a3", "c3", "b2"}
	if len(got) != len(want) {
		t.Fatalf("got %d reviews, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("order %v, want %v (ties by id)", mergedIDs(got), want)
		}
	}
	if len(mergeReviewLists(nil, 5)) != 0 {
		t.Fatal("no parts must merge to an empty list")
	}
}

func mergedIDs(rs []changereview.Review) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// Post-test audit: an unknown database is named as one Change Review does
// not run for, not as a missing review.
func TestChangeReviewFleetList_UnknownDatabaseMessage(t *testing.T) {
	f := newCRFleet(t)
	_, body := f.api.do(t, viewerUser(), "GET", crFleetPath+"?database=nope", "")
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "Change Review is not running") {
		t.Fatalf("error %q, want it to say Change Review is not running for it", msg)
	}
}
