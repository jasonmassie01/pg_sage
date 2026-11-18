package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// review-01a: GET /api/v1/databases/{db}/review/grade is the latest grade,
// the previous one and the delta between them, read-only, for a viewer.
// No concurrent access tests: the handler reads two rows; the store's
// concurrency is tested in rubric.

func reviewMux(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders",
		Config: config.DatabaseConfig{Name: "orders"}, Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "offline",
		Config: config.DatabaseConfig{Name: "offline"},
		Status: &fleet.InstanceStatus{}})
	mux := http.NewServeMux()
	registerReviewRoutes(mux, mgr)
	return mux
}

type reviewGradeBody struct {
	Latest   *rubric.Run     `json:"latest"`
	Previous *rubric.Run     `json:"previous"`
	Delta    *rubric.Changes `json:"delta"`
}

func seedGrade(t *testing.T, s *rubric.Store, results []rubric.CheckResult) {
	t.Helper()
	ctx := context.Background()
	id, err := s.Start(ctx, rubric.RunSpec{Database: "orders", Kind: rubric.KindGrade,
		RubricVersion: rubric.Version})
	if err != nil {
		t.Fatal(err)
	}
	g := rubric.Compute(rubric.V1(), results)
	if err := s.Finish(ctx, id, rubric.RunDone, &g, ""); err != nil {
		t.Fatal(err)
	}
}

func TestReviewGradeRouteDB_LatestPreviousAndDelta(t *testing.T) {
	pool := auditPool(t, "review_grade")
	h := reviewMux(t, pool)
	code, body := drillGet(t, h, "/api/v1/databases/orders/review/grade")
	var empty reviewGradeBody
	if code != http.StatusOK || json.Unmarshal(body, &empty) != nil || empty.Latest != nil ||
		empty.Delta != nil {
		t.Fatalf("no grade yet = %d %s, want 200 with a null latest", code, body)
	}
	s := rubric.NewStore(pool)
	seedGrade(t, s, nil)
	pass := []rubric.CheckResult{{Check: "schema.duplicate_index", Status: rubric.StatusPass,
		Evidence: []string{"first_look:1#duplicate_index"}}}
	seedGrade(t, s, pass)
	code, body = drillGet(t, h, "/api/v1/databases/orders/review/grade")
	var got reviewGradeBody
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("grade = %d %s", code, body)
	}
	if got.Latest == nil || got.Previous == nil || got.Latest.ID <= got.Previous.ID ||
		got.Latest.Grade.RubricVersion != rubric.Version || got.Delta == nil ||
		got.Delta.FirstRun || len(got.Delta.Dimensions) != 1 ||
		got.Delta.Dimensions[0].Dimension != "schema" {
		t.Fatalf("grade body = %s, want latest, previous and a schema-only delta", body)
	}
	moved := got.Delta.Dimensions[0].Checks
	if len(moved) != 1 || moved[0].From != rubric.StatusUnknown ||
		moved[0].To != rubric.StatusPass {
		t.Fatalf("moved = %+v, want duplicate_index unknown -> pass", moved)
	}
}

func TestReviewGradeRouteDB_FirstGradeDeltaIsTheFullReport(t *testing.T) {
	pool := auditPool(t, "review_grade_first")
	seedGrade(t, rubric.NewStore(pool), nil)
	code, body := drillGet(t, reviewMux(t, pool), "/api/v1/databases/orders/review/grade")
	var got reviewGradeBody
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Previous != nil ||
		got.Delta == nil || !got.Delta.FirstRun || len(got.Delta.Dimensions) != 8 {
		t.Fatalf("first grade = %d %s, want a first-run delta of 8 dimensions", code, body)
	}
}

func TestReviewGradeRoute_UnknownOrDisconnectedDatabase(t *testing.T) {
	h := reviewMux(t, nil)
	if code, _ := drillGet(t, h, "/api/v1/databases/nope/review/grade"); code !=
		http.StatusNotFound {
		t.Fatalf("unknown database = %d, want 404", code)
	}
	if code, _ := drillGet(t, h, "/api/v1/databases/offline/review/grade"); code !=
		http.StatusServiceUnavailable {
		t.Fatalf("disconnected database = %d, want 503", code)
	}
}

func TestReviewGradeMCPDB_ServesTheRouteRead(t *testing.T) {
	pool := auditPool(t, "review_grade_mcp")
	seedGrade(t, rubric.NewStore(pool), nil)
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders",
		Config: config.DatabaseConfig{Name: "orders"}, Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	backend := ReviewGradeMCP(mgr)
	got, err := backend.ReviewGrade(context.Background(), mcp.ReviewGradeRequest{
		Database: "orders"})
	body, ok := got.(reviewGradeResponse)
	if err != nil || !ok || body.Latest == nil || body.Delta == nil || !body.Delta.FirstRun {
		t.Fatalf("mcp read = %+v %v, want the first grade", got, err)
	}
	if _, err := backend.ReviewGrade(context.Background(),
		mcp.ReviewGradeRequest{Database: "nope"}); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown database = %v, want an error naming it", err)
	}
}
