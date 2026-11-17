package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// review-01b: POST /api/v1/databases/{db}/review/runs/{id}/viewed records
// the caller's first opening of a finished review in sage.review_view,
// once per run and user; it is the evidence that a person read it.

type viewedBody struct {
	RunID    int64     `json:"run_id"`
	ViewedAt time.Time `json:"viewed_at"`
	First    bool      `json:"first"`
}

func postViewed(t *testing.T, h http.Handler, path string, user *auth.User) (int, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func viewedPath(id int64) string {
	return fmt.Sprintf("/api/v1/databases/orders/review/runs/%d/viewed", id)
}

func seededRunID(t *testing.T, s *rubric.Store) int64 {
	t.Helper()
	seedGrade(t, s, nil)
	runs, err := s.Latest(context.Background(), "orders", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("latest = %v %v", runs, err)
	}
	return runs[0].ID
}

func TestReviewViewDB_RecordsTheFirstViewOncePerUser(t *testing.T) {
	pool := auditPool(t, "review_view_once")
	id := seededRunID(t, rubric.NewStore(pool))
	h := reviewMux(t, pool)
	code, body := postViewed(t, h, viewedPath(id), testViewerUser())
	var first viewedBody
	if code != http.StatusOK || json.Unmarshal(body, &first) != nil || !first.First ||
		first.RunID != id || first.ViewedAt.IsZero() {
		t.Fatalf("first view = %d %s, want 200 first", code, body)
	}
	code, body = postViewed(t, h, viewedPath(id), testViewerUser())
	var again viewedBody
	if code != http.StatusOK || json.Unmarshal(body, &again) != nil || again.First ||
		!again.ViewedAt.Equal(first.ViewedAt) {
		t.Fatalf("second view = %d %s, want 200, not first, the first time", code, body)
	}
	page := getReviewPage(t, h)
	if page.ViewedAt == nil || !page.ViewedAt.Equal(first.ViewedAt) {
		t.Fatalf("viewer's viewed_at = %v, want %v", page.ViewedAt, first.ViewedAt)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, withUser(httptest.NewRequest(http.MethodGet,
		"/api/v1/databases/orders/review/grade", nil), testOperatorUser()))
	var other reviewPageBody
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &other) != nil ||
		other.ViewedAt != nil {
		t.Fatalf("operator's viewed_at = %d %s, want null (not their view)", w.Code,
			w.Body.Bytes())
	}
}

func TestReviewViewDB_TwoTabsGiveOneRowPerUser(t *testing.T) {
	pool := auditPool(t, "review_view_tabs")
	id := seededRunID(t, rubric.NewStore(pool))
	h := reviewMux(t, pool)
	users := []*auth.User{testViewerUser(), testViewerUser(), testOperatorUser(),
		testOperatorUser()}
	codes := make([]int, len(users))
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = postViewed(t, h, viewedPath(id), u)
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("tab %d = %d, want 200", i, c)
		}
	}
	rows, err := pool.Query(context.Background(), `SELECT user_id, count(*)
		FROM sage.review_view WHERE run_id = $1 GROUP BY user_id ORDER BY user_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var user, n int
		if err := rows.Scan(&user, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%d", user, n))
	}
	if strings.Join(got, ",") != "2:1,3:1" {
		t.Fatalf("rows per user = %v, want one each for users 2 and 3", got)
	}
}

func TestReviewViewDB_RefusesWhatIsNotAFinishedReview(t *testing.T) {
	pool := auditPool(t, "review_view_refuse")
	s := rubric.NewStore(pool)
	done := seededRunID(t, s)
	running, err := s.Start(context.Background(), rubric.RunSpec{Database: "orders",
		Kind: rubric.KindGrade, RubricVersion: rubric.Version})
	if err != nil {
		t.Fatal(err)
	}
	h := reviewMux(t, pool)
	cases := []struct {
		name, path string
		user       *auth.User
		want       int
	}{
		{"running run", viewedPath(running), testViewerUser(), http.StatusConflict},
		{"no such run", viewedPath(done + 100), testViewerUser(), http.StatusNotFound},
		{"id not a number", "/api/v1/databases/orders/review/runs/abc/viewed",
			testViewerUser(), http.StatusBadRequest},
		{"id zero", viewedPath(0), testViewerUser(), http.StatusBadRequest},
		{"id negative", "/api/v1/databases/orders/review/runs/-4/viewed", testViewerUser(),
			http.StatusBadRequest},
		{"unknown database", "/api/v1/databases/nope/review/runs/1/viewed",
			testViewerUser(), http.StatusNotFound},
		{"disconnected database", "/api/v1/databases/offline/review/runs/1/viewed",
			testViewerUser(), http.StatusServiceUnavailable},
		{"user without an id", viewedPath(done), &auth.User{Role: auth.RoleViewer},
			http.StatusUnauthorized},
	}
	for _, c := range cases {
		if code, body := postViewed(t, h, c.path, c.user); code != c.want {
			t.Errorf("%s = %d %s, want %d", c.name, code, body, c.want)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.review_view`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("view rows = %d %v, want none", n, err)
	}
}

func TestReviewViewDB_StoreFailureIsA500WithoutTheDatabaseText(t *testing.T) {
	pool := auditPool(t, "review_view_fail")
	id := seededRunID(t, rubric.NewStore(pool))
	if _, err := pool.Exec(context.Background(), `DROP TABLE sage.review_view`); err != nil {
		t.Fatal(err)
	}
	code, body := postViewed(t, reviewMux(t, pool), viewedPath(id), testViewerUser())
	if code != http.StatusInternalServerError || strings.Contains(string(body), "relation") ||
		!strings.Contains(string(body), "could not be recorded") {
		t.Fatalf("store failure = %d %s, want a 500 naming what failed, no pgx text", code,
			body)
	}
	// The receipt never blocks the page: the grade still reads, with no
	// viewed_at, and the page's POST is the one that fails.
	page := getReviewPage(t, reviewMux(t, pool))
	if page.Latest == nil || page.Latest.ID != id || page.ViewedAt != nil {
		t.Fatalf("grade read without the view table = %+v, want the grade and no viewed_at",
			page)
	}
}
