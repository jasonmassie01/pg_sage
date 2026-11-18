package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/review/modelgate"
	"github.com/pg-sage/sidecar/internal/review/plan"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// review-05a: the scorecard route (viewer) and the gate approval (admin).
// The arithmetic and the gate's states are tested in internal/review; here
// the routes' contract, their errors and the golden body review-05b's mock
// copies. No concurrent access test: two approvals at once are serialized
// by the gate's lock (internal/review/modelgate).

const scorecardGolden = "testdata/review_scorecard.golden.json"

func gateMux(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders",
		Config: config.DatabaseConfig{Name: "orders"}, Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	mux := http.NewServeMux()
	registerReviewGateRoutes(mux, mgr, cfg)
	return mux
}

func gatePost(t *testing.T, h http.Handler, path string, user *auth.User) (int, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// seedGatePass stores 50 reviews of orders on m1 that pass G-M1 and
// evaluates the gate once; it returns the pending row's id.
func seedGatePass(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		gen := reviewtest.Outcome{Origin: "generator", Ref: "indexes", Verdict: "improved",
			Accepted: true}
		mod := reviewtest.Outcome{Origin: "model", Ref: "m1", Verdict: "improved",
			Accepted: true}
		if i%10 == 0 {
			mod.Verdict = "no_change"
		}
		if _, err := reviewtest.SeedReview(ctx, pool, reviewtest.Review{Database: "orders",
			Models: []string{"m1"}, Steps: []reviewtest.Outcome{gen, mod}}); err != nil {
			t.Fatal(err)
		}
	}
	g := modelgate.New(pool, "orders", modelgate.Options{
		Thresholds: modelgate.DefaultThresholds()})
	if _, err := g.Evaluate(ctx, "m1", time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := g.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending = %+v %v", rows, err)
	}
	return rows[0].ID
}

func TestReviewScorecardRouteDB_NumbersGateAndGolden(t *testing.T) {
	pool := auditPool(t, "review_scorecard")
	h := gateMux(t, pool)
	code, body := drillGet(t, h, "/api/v1/databases/orders/review/scorecard")
	var empty reviewScorecardResponse
	if code != http.StatusOK || json.Unmarshal(body, &empty) != nil ||
		empty.Scorecard.Reviews != 0 || len(empty.Gate) != 0 ||
		empty.Thresholds.MinReviews != 50 {
		t.Fatalf("no reviews = %d %s, want 200, zero reviews, no gate rows", code, body)
	}
	seedGatePass(t, pool)
	code, body = drillGet(t, h, "/api/v1/databases/orders/review/scorecard")
	var got reviewScorecardResponse
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("scorecard = %d %s", code, body)
	}
	m := got.Scorecard.ByOrigin["model"]
	if got.Scorecard.Reviews != 50 || m.Proposed != 50 || m.Improved != 45 ||
		m.NoChange != 5 || got.Scorecard.ByGenerator["indexes"].Improved != 50 ||
		len(got.Gate) != 2 || got.Gate[0].Rule != modelgate.RuleM1 ||
		got.Gate[0].Verdict != modelgate.StatePending {
		t.Fatalf("scorecard body = %s", body)
	}
	matchScorecardGolden(t, body)
	since := "?since=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, body = drillGet(t, h, "/api/v1/databases/orders/review/scorecard"+since)
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Scorecard.Reviews != 0 {
		t.Fatalf("since the future = %d %s, want no reviews", code, body)
	}
	code, _ = drillGet(t, h, "/api/v1/databases/orders/review/scorecard?since=yesterday")
	if code != http.StatusBadRequest {
		t.Fatalf("bad since = %d, want 400", code)
	}
}

func TestReviewGateApproveRouteDB_AdminGrantsOnceAndErrorsAreDistinct(t *testing.T) {
	pool := auditPool(t, "review_gate_approve")
	h := gateMux(t, pool)
	id := seedGatePass(t, pool)
	path := "/api/v1/databases/orders/review/gate/" + strconv.FormatInt(id, 10) + "/approve"
	if code, _ := gatePost(t, h, path, testOperatorUser()); code != http.StatusForbidden {
		t.Fatalf("operator approval = %d, want 403", code)
	}
	if live, _ := plan.ModelAuthority(context.Background(), pool, "m1"); live {
		t.Fatal("a refused approval granted authority")
	}
	before := inboxDecisions(inbox.KindAutonomyPromotion, "api")
	code, body := gatePost(t, h, path, testAdminUser())
	var row modelgate.Row
	if code != http.StatusOK || json.Unmarshal(body, &row) != nil ||
		row.Verdict != modelgate.StateGranted || row.ApprovedBy != "admin@test.com" {
		t.Fatalf("admin approval = %d %s", code, body)
	}
	if live, _ := plan.ModelAuthority(context.Background(), pool, "m1"); !live {
		t.Fatal("the approval did not set the authority flag")
	}
	if got := inboxDecisions(inbox.KindAutonomyPromotion, "api"); got != before+1 {
		t.Fatalf("inbox decisions = %d, want %d: the approval is one timed decision",
			got, before+1)
	}
	for name, c := range map[string]struct {
		path string
		want int
	}{
		"twice":   {path, http.StatusConflict},
		"unknown": {"/api/v1/databases/orders/review/gate/999999/approve", http.StatusNotFound},
		"bad id":  {"/api/v1/databases/orders/review/gate/x/approve", http.StatusBadRequest},
		"zero":    {"/api/v1/databases/orders/review/gate/0/approve", http.StatusBadRequest},
		"no db":   {"/api/v1/databases/nope/review/gate/1/approve", http.StatusNotFound},
	} {
		if code, body := gatePost(t, h, c.path, testAdminUser()); code != c.want {
			t.Errorf("%s = %d %s, want %d", name, code, body, c.want)
		}
	}
}

func TestReviewScorecardMCPDB_ServesTheRouteRead(t *testing.T) {
	pool := auditPool(t, "review_scorecard_mcp")
	seedGatePass(t, pool)
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders",
		Config: config.DatabaseConfig{Name: "orders"}, Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	got, err := ReviewScorecardMCP(mgr, cfg).ReviewScorecard(context.Background(),
		mcp.ReviewScorecardRequest{Database: "orders"})
	body, ok := got.(reviewScorecardResponse)
	if err != nil || !ok || body.Scorecard.Reviews != 50 || len(body.Gate) != 2 {
		t.Fatalf("mcp read = %+v %v, want the route's read", got, err)
	}
	_, err = ReviewScorecardMCP(mgr, cfg).ReviewScorecard(context.Background(),
		mcp.ReviewScorecardRequest{Database: "nope"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown database = %v, want an error naming it", err)
	}
}

// matchScorecardGolden compares body with the golden, times and ids masked
// (SAGE_UPDATE_REVIEW_SCORECARD_GOLDEN=1 rewrites it).
func matchScorecardGolden(t *testing.T, body []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	mask(v)
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SAGE_UPDATE_REVIEW_SCORECARD_GOLDEN") == "1" {
		if err := os.WriteFile(scorecardGolden, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(scorecardGolden)
	if err != nil {
		t.Fatalf("read %s (SAGE_UPDATE_REVIEW_SCORECARD_GOLDEN=1 writes it): %v",
			scorecardGolden, err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatalf("scorecard body differs from %s:\n%s", scorecardGolden, got)
	}
}

// mask replaces every time and id, which differ per run, with a marker.
func mask(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch {
			case k == "id" || k == "since" || k == "tick" || strings.HasSuffix(k, "_at"):
				x[k] = "<" + k + ">"
			default:
				mask(val)
			}
		}
	case []any:
		for _, e := range x {
			mask(e)
		}
	}
}
