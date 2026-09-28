package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

type recAPIFixture struct {
	pool   *pgxpool.Pool
	mgr    *fleet.DatabaseManager
	dbName string
	rec    recommendation.Recommendation
	find   int
	p      recommendation.Proposal
}

func newRecAPIFixture(t *testing.T) recAPIFixture {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	name := fmt.Sprintf("recapi_%d", time.Now().UnixNano())
	target := "public." + name
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql)
		VALUES ('rec_api', 'warning', 'index', $1, 't', '{}',
		        'CREATE INDEX CONCURRENTLY a ON t (a)') RETURNING id`, target).
		Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	p := recommendation.Proposal{DatabaseName: name, Category: "rec_api", Target: target,
		ForwardSQL: "CREATE INDEX CONCURRENTLY a ON t (a)",
		InverseSQL: "DROP INDEX CONCURRENTLY a"}
	res, err := recommendation.NewStore(pool).Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool})
	return recAPIFixture{pool: pool, mgr: mgr, dbName: name, rec: res.Recommendation,
		find: findingID, p: p}
}

func TestRecommendationsListHandler(t *testing.T) {
	fx := newRecAPIFixture(t)
	w := doRequest(recommendationsListHandler(fx.mgr), http.MethodGet,
		"/api/v1/recommendations?database="+fx.dbName, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Database        string                          `json:"database"`
		Recommendations []recommendation.Recommendation `json:"recommendations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Database != fx.dbName || len(body.Recommendations) != 1 ||
		body.Recommendations[0].ID != fx.rec.ID ||
		body.Recommendations[0].State != recommendation.StateProposed {
		t.Fatalf("list body = %+v", body)
	}
	w = doRequest(recommendationsListHandler(fx.mgr), http.MethodGet,
		"/api/v1/recommendations?database="+fx.dbName+"&state=bogus", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bogus state: status %d, want 400", w.Code)
	}
	w = doRequest(recommendationsListHandler(fx.mgr), http.MethodGet,
		"/api/v1/recommendations?database=nope_db", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown database: status %d, want 404", w.Code)
	}
}

func detailRequest(fx recAPIFixture, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/recommendations/"+id+"?database="+fx.dbName, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	recommendationDetailHandler(fx.mgr).ServeHTTP(w, req)
	return w
}

func TestRecommendationDetailHandlerShowsRevisionsAndHistory(t *testing.T) {
	fx := newRecAPIFixture(t)
	ctx := t.Context()
	recs := recommendation.NewStore(fx.pool)
	if _, err := recs.Approve(ctx, fx.rec.ID, fx.rec.ContentHash, "user:2"); err != nil {
		t.Fatal(err)
	}
	fx.p.InverseSQL = "DROP INDEX CONCURRENTLY public.a"
	if _, err := recs.Propose(ctx, fx.p); err != nil {
		t.Fatal(err)
	}
	w := detailRequest(fx, strconv.FormatInt(fx.rec.ID, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Recommendation recommendation.Recommendation `json:"recommendation"`
		Revisions      []recommendation.Revision     `json:"revisions"`
		Transitions    []recommendation.Transition   `json:"transitions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Recommendation.Revision != 2 || len(body.Revisions) != 2 ||
		body.Revisions[1].InverseSQL != "DROP INDEX CONCURRENTLY public.a" ||
		len(body.Transitions) != 3 || body.Transitions[1].Actor != "user:2" {
		t.Fatalf("detail = %+v", body)
	}
	if w := detailRequest(fx, "abc"); w.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric id: status %d, want 400", w.Code)
	}
	if w := detailRequest(fx, "999999999"); w.Code != http.StatusNotFound {
		t.Fatalf("missing id: status %d, want 404", w.Code)
	}
}

// C04 at the HTTP surface: approving a queued proposal whose
// recommendation was revised is a conflict, not a stale execution.
func TestApproveHandlerRefusesRevisedRecommendation(t *testing.T) {
	fx := newRecAPIFixture(t)
	ctx := t.Context()
	as := store.NewActionStore(fx.pool)
	queueID, err := as.ProposeWithMetadata(ctx, nil, fx.find, fx.p.ForwardSQL,
		fx.p.InverseSQL, "safe", store.ActionProposalMetadata{
			RecommendationID: fx.rec.ID, RecommendationRevision: fx.rec.Revision,
			ContentHash: fx.rec.ContentHash})
	if err != nil {
		t.Fatal(err)
	}
	fx.p.InverseSQL = "DROP INDEX CONCURRENTLY public.a"
	if _, err := recommendation.NewStore(fx.pool).Propose(ctx, fx.p); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(ctx, `UPDATE sage.action_queue SET status='pending'
		WHERE id=$1`, queueID); err != nil {
		t.Fatal(err)
	}
	id := strconv.Itoa(queueID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions/"+id+"/approve", nil)
	req.SetPathValue("id", id)
	req = withUser(req, testOperatorUser())
	w := httptest.NewRecorder()
	approveActionHandler(as, nil).ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("approve revised: status %d body=%s, want 409", w.Code, w.Body.String())
	}
	var status string
	_ = fx.pool.QueryRow(ctx, `SELECT status FROM sage.action_queue WHERE id=$1`,
		queueID).Scan(&status)
	if status != "pending" {
		t.Fatalf("queue row status after refused approval = %q, want pending", status)
	}
}
