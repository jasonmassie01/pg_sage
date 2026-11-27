package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// review-01b, reviewed: the read receipt goes through the real router
// with database grants on. A viewer granted billing records a receipt on
// billing; on search, a database they cannot read, the POST answers like
// an unknown database (404) and writes nothing there.
// No concurrent access tests: TestReviewViewDB_TwoTabsGiveOneRowPerUser
// races the same handler; this one checks only who may reach it.

func seedFleetGrade(t *testing.T, mgr *fleet.DatabaseManager, db string) int64 {
	t.Helper()
	ctx := context.Background()
	s := rubric.NewStore(mgr.GetInstance(db).Pool)
	id, err := s.Start(ctx, rubric.RunSpec{Database: db, Kind: rubric.KindGrade,
		RubricVersion: rubric.Version})
	if err != nil {
		t.Fatalf("start a grade of %s: %v", db, err)
	}
	g := rubric.Compute(rubric.V1(), nil)
	if err := s.Finish(ctx, id, rubric.RunDone, &g, ""); err != nil {
		t.Fatalf("finish the grade of %s: %v", db, err)
	}
	return id
}

func countViews(t *testing.T, mgr *fleet.DatabaseManager, db string) int {
	t.Helper()
	var n int
	if err := mgr.GetInstance(db).Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.review_view`).Scan(&n); err != nil {
		t.Fatalf("count the views of %s: %v", db, err)
	}
	return n
}

func postScoped(h http.Handler, path string) (int, string) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestReviewViewDB_ScopedViewerRecordsOnlyOnAGrantedDatabase(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	adminID := phase2EnsureUser(t, pool, ctx)
	scopedID, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	if _, err := auth.NewDatabaseGrantStore(pool).SetGrant(ctx, auth.GrantChange{
		ActorID: adminID, UserID: scopedID, Database: "billing",
		Role: auth.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	mgr := readFleetDB(t)
	billing, search := seedFleetGrade(t, mgr, "billing"), seedFleetGrade(t, mgr, "search")
	h := grantsRouter(t, pool, mgr, &auth.User{ID: scopedID, Role: auth.RoleViewer})
	path := "/api/v1/databases/%s/review/runs/%d/viewed"

	hc, hb := postScoped(h, fmt.Sprintf(path, "search", search))
	uc, ub := postScoped(h, fmt.Sprintf(path, "nosuch", search))
	if hc != http.StatusNotFound || hc != uc || hb != ub {
		t.Fatalf("hidden search %d %s vs unknown %d %s, want identical 404s", hc, hb, uc, ub)
	}
	if n := countViews(t, mgr, "search"); n != 0 {
		t.Fatalf("search has %d read receipts after a refused POST, want 0", n)
	}
	code, body := postScoped(h, fmt.Sprintf(path, "billing", billing))
	var v viewedBody
	if code != http.StatusOK || json.Unmarshal([]byte(body), &v) != nil || !v.First ||
		v.RunID != billing {
		t.Fatalf("granted billing = %d %s, want 200 first view of run %d", code, body,
			billing)
	}
	if n := countViews(t, mgr, "billing"); n != 1 {
		t.Fatalf("billing has %d read receipts, want 1", n)
	}
}
