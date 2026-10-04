package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 2.2: GET /api/v1/tuning/calibration?database= serves the tuning
// agent's calibration: per action class and prediction method, the
// reliability bins mapping predicted improvement to observed outcomes.

func calibrationGet(t *testing.T, pool *pgxpool.Pool, query string) (int, map[string]any) {
	t.Helper()
	return calibrationGetWith(t, phase2MgrWithPool(pool), query)
}

func calibrationGetWith(t *testing.T, mgr *fleet.DatabaseManager, query string) (int,
	map[string]any) {
	t.Helper()
	h := tuningCalibrationHandler(mgr)
	req := httptest.NewRequest("GET", "/api/v1/tuning/calibration"+query, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func seedCalibrationOutcomes(t *testing.T, pool *pgxpool.Pool, ctx context.Context, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := insertLedgerLogRow(t, pool, ctx, "success")
		recordTestOutcome(t, pool, ctx, id, verify.OutcomeImproved)
	}
}

func TestTuningCalibration_ServesReliabilityBins(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.action_outcome"); err != nil {
		t.Fatalf("clean outcomes: %v", err)
	}
	seedCalibrationOutcomes(t, pool, ctx, 5)
	code, body := calibrationGet(t, pool, "?database=testdb")
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	if body["database"] != "testdb" || body["min_outcomes"] != float64(5) {
		t.Fatalf("body = %v", body)
	}
	classes, _ := body["classes"].([]any)
	if len(classes) != 1 {
		t.Fatalf("classes = %v", body["classes"])
	}
	c, _ := classes[0].(map[string]any)
	if c["class"] != verify.ClassIndexCreate || c["method"] != verify.MethodHypoPG ||
		c["n"] != float64(5) || c["hits"] != float64(5) || c["status"] != "calibrated" {
		t.Fatalf("class = %v", c)
	}
	bins, _ := c["bins"].([]any)
	if len(bins) != 4 {
		t.Fatalf("bins = %v", c["bins"])
	}
	b, _ := bins[2].(map[string]any)
	if b["label"] != "25-50" || b["n"] != float64(5) || b["tolerance_met"] != float64(5) {
		t.Fatalf("bin 25-50 = %v", b)
	}
}

func TestTuningCalibration_EmptyAndBadRequests(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.action_outcome"); err != nil {
		t.Fatalf("clean outcomes: %v", err)
	}
	code, body := calibrationGet(t, pool, "?database=testdb")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if classes, ok := body["classes"].([]any); !ok || len(classes) != 0 {
		t.Fatalf("no outcomes is an empty list, not null: %v", body["classes"])
	}
	if code, _ := calibrationGet(t, pool, ""); code != http.StatusBadRequest {
		t.Fatalf("missing database: %d", code)
	}
	if code, _ := calibrationGet(t, pool, "?database=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown database: %d", code)
	}
}

type statsOnlyTuning struct{ stats analyzer.TuningStats }

func (s statsOnlyTuning) Tune(context.Context, *collector.Snapshot, *collector.Snapshot) (
	analyzer.TuningOutput, error) {
	return analyzer.TuningOutput{}, nil
}

func (s statsOnlyTuning) Stats() analyzer.TuningStats { return s.stats }

// The calibration response carries the agent's last-cycle budget use, so
// the Trust page shows what was spent and how many cases wait.
func TestTuningCalibration_ServesTheLastCycleBudget(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	mgr := phase2MgrWithPool(pool)
	code, body := calibrationGetWith(t, mgr, "?database=testdb")
	if code != http.StatusOK || body["budget"] != nil {
		t.Fatalf("no agent, no budget: %d %v", code, body["budget"])
	}
	tp := statsOnlyTuning{stats: analyzer.TuningStats{TokensUsed: 41000, TokenLimit: 60000,
		RequestsUsed: 12, RequestLimit: 12, CasesAsked: 2, CasesDeferred: 5,
		DayTokensUsed: 480000, DayTokenLimit: 500000}}
	mgr.GetInstance("testdb").Analyzer = analyzer.New(nil, mgr.Config(), nil, tp, nil, nil,
		nil, func(string, string, ...any) {})
	code, body = calibrationGetWith(t, mgr, "?database=testdb")
	b, _ := body["budget"].(map[string]any)
	if code != http.StatusOK || b["tokens_used"] != float64(41000) ||
		b["token_limit"] != float64(60000) || b["requests_used"] != float64(12) ||
		b["request_limit"] != float64(12) || b["cases_asked"] != float64(2) ||
		b["cases_deferred"] != float64(5) || b["day_tokens_used"] != float64(480000) ||
		b["day_token_limit"] != float64(500000) {
		t.Fatalf("budget = %v (%d)", body["budget"], code)
	}
}
