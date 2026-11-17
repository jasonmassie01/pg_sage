package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// ux-03 on real tables: GET /api/v1/cases gives a finding case whose
// finding is a step of a plan plans: [{id, goal}], read through the plan
// store; a case of another finding has none.

// seedCaseFindings inserts open findings 11 (public.orders) and 12
// (public.items).
func seedCaseFindings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for id, object := range map[int64]string{11: "public.orders", 12: "public.items"} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO sage.findings
			(id, category, severity, object_type, object_identifier, title, detail,
			 recommendation, recommended_sql, status)
			VALUES ($1, 'stale_statistics', 'warning', 'table', $2, 'stale stats', '{}',
			        'analyze it', 'ANALYZE '||$2, 'open')`, id, object); err != nil {
			t.Fatalf("seed finding %d: %v", id, err)
		}
	}
}

type casePlansBody struct {
	Cases []struct {
		SourceIDs        []string         `json:"source_ids"`
		Plans            []map[string]any `json:"plans"`
		PlansUnavailable bool             `json:"plans_unavailable"`
	} `json:"cases"`
}

func TestCasesPlansDB_FindingCaseNamesItsPlan(t *testing.T) {
	pool := planDB(t, "cases_plans_api")
	seedCaseFindings(t, pool)
	p := twoStepPlan(t, pool, "speed up the order lookups") // finding 11
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	rec := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/cases?database=orders", nil))
	var body casePlansBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("cases: %d %v %s", rec.Code, err, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, c := range body.Cases {
		if len(c.SourceIDs) == 0 || c.PlansUnavailable {
			t.Fatalf("case %+v: no source or plans unavailable", c)
		}
		seen[c.SourceIDs[0]] = true
		ok := c.SourceIDs[0] != "11" || (len(c.Plans) == 1 &&
			c.Plans[0]["id"] == float64(p.ID) && c.Plans[0]["goal"] == "speed up the order lookups")
		if !ok || (c.SourceIDs[0] == "12" && c.Plans != nil) {
			t.Fatalf("finding %s plans = %v; want plan %d for 11 only", c.SourceIDs[0], c.Plans,
				p.ID)
		}
	}
	if !seen["11"] || !seen["12"] {
		t.Fatalf("cases %v, want findings 11 and 12", seen)
	}
}
