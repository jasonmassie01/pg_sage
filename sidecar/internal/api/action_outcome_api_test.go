package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Phase 1.3: predicted vs observed is persisted per action and served by
// the actions API (list and detail) and by the outcome ledger the trust
// system reads.

func recordTestOutcome(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, id int64, verdict string,
) {
	t.Helper()
	store := verify.NewOutcomeStore(pool)
	expected := -40.0
	if err := store.RecordPrediction(ctx, id, verify.Prediction{
		Class: verify.ClassIndexCreate, Method: verify.MethodHypoPG,
		Metric: verify.MetricMeanExecTime, TargetQueryIDs: []int64{77},
		ExpectedChangePct: &expected, Source: "optimizer"}); err != nil {
		t.Fatalf("RecordPrediction: %v", err)
	}
	observed := -55.0
	start, end := time.Now().Add(-time.Hour).UTC(), time.Now().UTC()
	if err := store.RecordVerdict(ctx, verify.Outcome{ActionLogID: id, Verdict: verdict,
		Observed: verify.Observed{Metric: verify.MetricMeanExecTime, Before: 20, After: 9,
			ChangePct: &observed},
		Evidence: map[string]any{"comparison": map[string]any{"t": -12.5}},
		Reason:   "call-weighted mean fell", WindowStart: &start, WindowEnd: &end,
	}); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
}

func assertServedOutcome(t *testing.T, row map[string]any, verdict string) {
	t.Helper()
	o, ok := row["verification_outcome"].(map[string]any)
	if !ok {
		t.Fatalf("verification_outcome missing from %v", row)
	}
	if o["verdict"] != verdict || o["tolerance"] != verify.ToleranceMet ||
		o["class"] != verify.ClassIndexCreate || o["reason"] == "" {
		t.Fatalf("served outcome = %v", o)
	}
	predicted, _ := o["predicted"].(map[string]any)
	observed, _ := o["observed"].(map[string]any)
	if predicted["expected_change_pct"] != -40.0 || predicted["method"] != "hypopg" {
		t.Fatalf("predicted = %v", predicted)
	}
	if observed["change_pct"] != -55.0 || observed["before"] != 20.0 {
		t.Fatalf("observed = %v", observed)
	}
	if _, ok := o["evidence"].(map[string]any); !ok {
		t.Fatalf("evidence missing: %v", o)
	}
}

func TestActionDetailServesPredictedVsObserved(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	id := insertLedgerLogRow(t, pool, ctx, "success")
	recordTestOutcome(t, pool, ctx, id, verify.OutcomeImproved)

	handler := actionDetailHandler(phase2MgrWithPool(pool))
	req := httptest.NewRequest("GET", "/api/v1/actions/"+strconv.FormatInt(id, 10)+
		"?database=testdb", nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var row map[string]any
	if err := json.NewDecoder(w.Body).Decode(&row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertServedOutcome(t, row, verify.OutcomeImproved)
}

func TestActionsListServesPredictedVsObserved(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	verified := insertLedgerLogRow(t, pool, ctx, "success")
	plain := insertLedgerLogRow(t, pool, ctx, "failed")
	recordTestOutcome(t, pool, ctx, verified, verify.OutcomeImproved)

	resp := getLedger(t, pool)
	seen := 0
	for _, raw := range resp["actions"].([]any) {
		row := raw.(map[string]any)
		switch row["id"] {
		case strconv.FormatInt(verified, 10):
			assertServedOutcome(t, row, verify.OutcomeImproved)
			seen++
		case strconv.FormatInt(plain, 10):
			if row["verification_outcome"] != nil {
				t.Fatalf("action without an outcome served %v", row["verification_outcome"])
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 actions", seen)
	}
}

func getOutcomes(t *testing.T, pool *pgxpool.Pool, query string) (int, map[string]any) {
	t.Helper()
	handler := actionOutcomesHandler(phase2MgrWithPool(pool))
	req := httptest.NewRequest("GET", "/api/v1/action-outcomes?database=testdb"+query, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp
}

func TestActionOutcomesLedgerFilters(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	improved := insertLedgerLogRow(t, pool, ctx, "success")
	regressed := insertLedgerLogRow(t, pool, ctx, "rolled_back")
	recordTestOutcome(t, pool, ctx, improved, verify.OutcomeImproved)
	recordTestOutcome(t, pool, ctx, regressed, verify.OutcomeRegressed)

	code, resp := getOutcomes(t, pool, "&class=index_create&verdict=regressed")
	if code != http.StatusOK {
		t.Fatalf("status = %d resp = %v", code, resp)
	}
	outcomes, _ := resp["outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %v, want only the regressed one", outcomes)
	}
	o := outcomes[0].(map[string]any)
	if o["action_log_id"] != float64(regressed) || o["verdict"] != verify.OutcomeRegressed {
		t.Fatalf("outcome = %v", o)
	}
	if resp["database"] != "testdb" {
		t.Fatalf("database = %v", resp["database"])
	}
	code, resp = getOutcomes(t, pool, "")
	if all, _ := resp["outcomes"].([]any); code != http.StatusOK || len(all) != 2 {
		t.Fatalf("unfiltered = %d %v, want both outcomes", code, resp)
	}
}

func TestActionOutcomesLedgerRejectsBadInput(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	for _, query := range []string{"&limit=0", "&limit=5000", "&limit=x",
		"&since=yesterday", "&verdict=success"} {
		code, resp := getOutcomes(t, pool, query)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d (%v), want 400", query, code, resp)
		}
	}
}

func TestActionOutcomesLedgerRequiresKnownDatabase(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	handler := actionOutcomesHandler(phase2MgrWithPool(pool))
	req := httptest.NewRequest("GET", "/api/v1/action-outcomes?database=nope", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("unknown database served %s", w.Body.String())
	}
}
