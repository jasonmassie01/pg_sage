package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-07: GET /api/v1/policy/explain answers "why hasn't this run?"
// for one finding's case candidates, through the executor's explain path
// (cases-04), and records nothing.

type explainBody struct {
	Database   string           `json:"database"`
	FindingID  string           `json:"finding_id"`
	Status     string           `json:"status"`
	Candidates []map[string]any `json:"candidates"`
}

func findingIDFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, object string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM sage.findings
		WHERE object_identifier = $1 ORDER BY id DESC LIMIT 1`, object).Scan(&id); err != nil {
		t.Fatalf("finding id for %s: %v", object, err)
	}
	return id
}

func decisionCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.decision").Scan(&n); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	return n
}

func getExplain(t *testing.T, mux http.Handler, query string) (int, explainBody, string) {
	t.Helper()
	rec := requestPolicy(mux, http.MethodGet, "/api/v1/policy/explain?"+query, "",
		testOperatorUser())
	var body explainBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec.Code, body, rec.Body.String()
}

// Happy path: the candidate's verdict is the executor's for its finding,
// and explaining writes no decision row (CHECK-14).
func TestPolicyExplainMatchesTheExecutorAndRecordsNothing(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, exec := providerCaseManager(t, ctx, pool, "")
	sql := "ALTER SYSTEM SET work_mem = '64MB'"
	seedFinding(t, ctx, "config", "setting", "work_mem", sql)
	id := findingIDFor(t, ctx, pool, "work_mem")
	before := decisionCount(t, ctx, pool)
	mux := decisionsMux(mgr)
	code, body, raw := getExplain(t, mux, "database=prod&finding_id="+strconv.FormatInt(id, 10))
	if code != http.StatusOK || body.Status != "explained" || len(body.Candidates) != 1 {
		t.Fatalf("explain: %d %s, want one explained candidate", code, raw)
	}
	contract, _ := executor.ContractForActionType("alter_system_guc")
	want := exec.ExplainActions(ctx, []executor.ExplainActionRequest{{Contract: contract,
		Finding: findingForTest("config", "setting", "work_mem", sql)}}, false)[0]
	got := body.Candidates[0]
	if got["action_type"] != "alter_system_guc" || got["decision"] != want.Decision ||
		stringValue(got["blocked_reason"]) != want.BlockedReason ||
		got["risk_tier"] != want.RiskTier {
		t.Fatalf("candidate = %v, want the executor's %s/%q", got, want.Decision,
			want.BlockedReason)
	}
	if body.Database != "prod" || body.FindingID != strconv.FormatInt(id, 10) ||
		got["human_reason"] == nil {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(raw, "ALTER SYSTEM") {
		t.Fatalf("explain returned the SQL text: %s", raw)
	}
	if after := decisionCount(t, ctx, pool); after != before {
		t.Fatalf("sage.decision rows %d -> %d: explain recorded a decision", before, after)
	}
}

// Nil/empty: a finding with no candidate is no_candidate with [].
func TestPolicyExplainNoCandidate(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, _ := providerCaseManager(t, ctx, pool, "")
	seedFinding(t, ctx, "test_no_candidate", "table", "public.nothing_to_do", "")
	id := findingIDFor(t, ctx, pool, "public.nothing_to_do")
	code, body, raw := getExplain(t, decisionsMux(mgr),
		"database=prod&finding_id="+strconv.FormatInt(id, 10))
	if code != http.StatusOK || body.Status != "no_candidate" || body.Candidates == nil ||
		len(body.Candidates) != 0 || !strings.Contains(raw, `"candidates":[]`) {
		t.Fatalf("no candidate: %d %s", code, raw)
	}
}

// Invalid input and missing rows: a bad finding_id is 400, an unknown
// finding or database 404, and a fleet request without a database 400.
func TestPolicyExplainInvalidAndMissing(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, _ := providerCaseManager(t, ctx, pool, "")
	mux := decisionsMux(mgr)
	for query, want := range map[string]int{
		"database=prod&finding_id=abc":    http.StatusBadRequest,
		"database=prod&finding_id=-4":     http.StatusBadRequest,
		"database=prod&finding_id=0":      http.StatusBadRequest,
		"database=prod":                   http.StatusBadRequest,
		"database=prod&finding_id=999999": http.StatusNotFound,
		"database=nope&finding_id=1":      http.StatusNotFound,
	} {
		code, _, raw := getExplain(t, mux, query)
		if code != want {
			t.Errorf("%s: %d %s, want %d", query, code, raw, want)
		}
		if want == http.StatusBadRequest && strings.Contains(query, "finding_id=") &&
			!strings.Contains(raw, "finding_id") {
			t.Errorf("%s: %s does not name finding_id", query, raw)
		}
	}
}

// Concurrent access: parallel explains while the gate records decisions
// through the standing gate; every explain and list answers 200.
func TestPolicyExplainConcurrentWithRecording(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, exec := providerCaseManager(t, ctx, pool, "")
	sql := "ALTER SYSTEM SET work_mem = '32MB'"
	seedFinding(t, ctx, "config", "setting", "work_mem", sql)
	id := strconv.FormatInt(findingIDFor(t, ctx, pool, "work_mem"), 10)
	mux := decisionsMux(mgr)
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			code, _, _ := getExplain(t, mux, "database=prod&finding_id="+id)
			codes <- code
		}()
		go func() {
			defer wg.Done()
			_ = exec.StandingPolicyGate().Authorize(ctx, policy.ActionRequest{
				SQL: sql, Feature: "config", TargetObjs: []string{"work_mem"}})
			rec := requestPolicy(mux, http.MethodGet, "/api/v1/decisions?database=prod", "",
				testOperatorUser())
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent request status %d", code)
		}
	}
}
