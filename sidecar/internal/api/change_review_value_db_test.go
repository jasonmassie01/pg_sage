package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/value"
)

// CHECK-RCP-06 at the API: GET /api/v1/value shows a final Change Review
// credit as by_feature.change_review (minutes / 60) and the pending
// review minutes in potential_hours_pending.

func TestValueRouteShowsChangeReviewCredit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "api_cr_value"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.change_review (database_name, source,
		actor, input_hash, state, stage, verdict, deterministic_verdict, parse_mode,
		model_status, landing_state, landing_source, landed_lower, landed_at,
		receipt_verdict, dba_verdict, dba_actor, credit_state, credited_minutes,
		toil_model_version, credit_final_at, completed_at)
		SELECT 'db', 'mcp', 'a', md5(g::text) || md5(g::text), 'complete', 'done', 'safe',
		'safe', 'ast', 'no_model', 'landed', 'reported', now(), now(),
		CASE WHEN g = 1 THEN 'clean' ELSE 'pending' END, 'concur', 'user:1',
		CASE WHEN g = 1 THEN 'final' ELSE 'pending' END, 15, 1,
		CASE WHEN g = 1 THEN now() END, now()
		FROM generate_series(1, 3) g`); err != nil {
		t.Fatalf("seed reviews: %v", err)
	}
	reader := value.NewFleetService(func() []value.Source {
		return []value.Source{{Name: "db", Pool: pool}}
	})
	rec := httptest.NewRecorder()
	valueHandler(reader).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/value",
		nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var rep value.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if got := rep.ByFeature["change_review"]; math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("by_feature.change_review = %v, want 0.25", got)
	}
	if math.Abs(rep.PotentialHoursPending-0.5) > 1e-9 {
		t.Fatalf("potential_hours_pending = %v, want 0.5", rep.PotentialHoursPending)
	}
}
