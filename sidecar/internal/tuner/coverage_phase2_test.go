package tuner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func noopLog2(_ string, _ string, _ ...any) {}

// ---------------------------------------------------------------------------
// connectTestDB helper
// ---------------------------------------------------------------------------

func connectTunerTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("DB unavailable: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("DB ping failed: %v", err)
	}
	return pool
}

// ---------------------------------------------------------------------------
// fetchCandidates (was 16.7%) — test with pg_stat_statements data
// ---------------------------------------------------------------------------

func TestPhase2_FetchCandidates_NoStatStatements(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	// Check if pg_stat_statements is available.
	var exists bool
	err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')",
	).Scan(&exists)
	if err != nil || !exists {
		t.Skipf("pg_stat_statements not available")
	}

	tu := New(pool, TunerConfig{
		MinQueryCalls: 999999, // Very high threshold -> 0 candidates
		PlanTimeRatio: 0.5,
	}, nil, noopLog2)

	candidates, err := tu.fetchCandidates(context.Background())
	if err != nil {
		t.Fatalf("fetchCandidates: %v", err)
	}
	// With such a high MinQueryCalls, should return 0 candidates.
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates with high threshold, "+
			"got %d", len(candidates))
	}
}

func TestPhase2_FetchCandidates_WithData(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	var exists bool
	err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')",
	).Scan(&exists)
	if err != nil || !exists {
		t.Skipf("pg_stat_statements not available")
	}

	tu := New(pool, TunerConfig{
		MinQueryCalls: 1,
		PlanTimeRatio: 0.5,
	}, nil, noopLog2)

	candidates, err := tu.fetchCandidates(context.Background())
	if err != nil {
		t.Fatalf("fetchCandidates: %v", err)
	}
	// We can't predict exact count, but verify no error.
	_ = candidates
}

func TestPhase2_FetchCandidates_CancelledContext(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	tu := New(pool, TunerConfig{
		MinQueryCalls: 1,
		PlanTimeRatio: 0.5,
	}, nil, noopLog2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tu.fetchCandidates(ctx)
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Tune (was 23.1%) — test main entry point
// ---------------------------------------------------------------------------

func TestPhase2_Tune_NoCandidates(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	var exists bool
	_ = pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')",
	).Scan(&exists)
	if !exists {
		t.Skipf("pg_stat_statements not available")
	}

	tu := New(pool, TunerConfig{
		MinQueryCalls: 999999,
		PlanTimeRatio: 0.5,
	}, nil, noopLog2)

	findings, err := tu.Tune(context.Background())
	if err != nil {
		t.Fatalf("Tune: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings with no candidates, "+
			"got %d", len(findings))
	}
}

func TestPhase2_Tune_CooldownSkip(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	var exists bool
	_ = pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')",
	).Scan(&exists)
	if !exists {
		t.Skipf("pg_stat_statements not available")
	}

	tu := New(pool, TunerConfig{
		MinQueryCalls:         1,
		PlanTimeRatio:         0.5,
		CascadeCooldownCycles: 10,
	}, nil, noopLog2)

	// Pre-seed recentlyTuned so all candidates are skipped.
	// We need to get real candidate IDs first.
	candidates, err := tu.fetchCandidates(context.Background())
	if err != nil || len(candidates) == 0 {
		t.Skipf("no candidates available for cooldown test")
	}
	for _, c := range candidates {
		tu.recentlyTuned[c.QueryID] = 5
	}

	findings, err := tu.Tune(context.Background())
	if err != nil {
		t.Fatalf("Tune: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (all cooled down), "+
			"got %d", len(findings))
	}
}

func TestPhase2_Tune_TickCooldowns(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	tu := New(pool, TunerConfig{
		MinQueryCalls: 999999, // No candidates
		PlanTimeRatio: 0.5,
	}, nil, noopLog2)

	// Pre-seed a cooldown that will expire.
	tu.recentlyTuned[12345] = 1

	_, _ = tu.Tune(context.Background())

	// After tickCooldowns, entry at 1 should be deleted.
	if _, ok := tu.recentlyTuned[12345]; ok {
		t.Error("cooldown at 1 should be removed after tick")
	}
}

func TestPhase2_Tune_TickCooldowns_Decrement(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	tu := New(pool, TunerConfig{
		MinQueryCalls: 999999,
		PlanTimeRatio: 0.5,
	}, nil, noopLog2)

	tu.recentlyTuned[99999] = 3

	_, _ = tu.Tune(context.Background())

	remaining, ok := tu.recentlyTuned[99999]
	if !ok {
		t.Fatal("entry should still exist (was 3, now 2)")
	}
	if remaining != 2 {
		t.Errorf("expected 2 remaining, got %d", remaining)
	}
}

// ---------------------------------------------------------------------------
// tryLLMPrescribe (was 18.2%) — test with httptest LLM mock
// ---------------------------------------------------------------------------

func TestPhase2_TryLLMPrescribe_NilClient(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	tu := New(pool, TunerConfig{}, nil, noopLog2)
	// No LLM client -> should return nil.
	rx := tu.tryLLMPrescribe(
		context.Background(),
		candidate{QueryID: 1, Query: "SELECT 1"},
		[]PlanSymptom{{Kind: SymptomDiskSort}},
		`Set(work_mem "64MB")`,
	)
	if rx != nil {
		t.Errorf("expected nil with no LLM client, got %v", rx)
	}
}

func TestPhase2_TryLLMPrescribe_WithMockLLM(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	// Create a mock LLM server that returns valid prescriptions.
	mockResp := `{
		"choices": [{
			"message": {
				"content": "[{\"hint_directive\": \"Set(work_mem \\\"256MB\\\")\", \"rationale\": \"increase work_mem\", \"confidence\": 0.8}]"
			}
		}],
		"usage": {"total_tokens": 100}
	}`
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, mockResp)
		},
	))
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		Model:            "test-model",
		APIKey:           "test-key",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
	}, noopLog2)

	var logMsgs []string
	logFn := func(_ string, msg string, args ...any) {
		logMsgs = append(logMsgs, fmt.Sprintf(msg, args...))
	}

	tu := New(pool, TunerConfig{}, nil, logFn, WithLLM(client, nil))

	rx := tu.tryLLMPrescribe(
		context.Background(),
		candidate{
			QueryID:      1,
			Query:        "SELECT * FROM orders WHERE status = 'pending'",
			Calls:        100,
			MeanExecTime: 500.0,
		},
		[]PlanSymptom{{Kind: SymptomDiskSort}, {Kind: SymptomHashSpill}},
		`Set(work_mem "64MB")`,
	)

	if len(rx) == 0 {
		for _, m := range logMsgs {
			t.Logf("LOG: %s", m)
		}
		t.Fatal("expected prescriptions from mock LLM")
	}
	if rx[0].HintDirective == "" {
		t.Error("hint directive should not be empty")
	}
}

func TestPhase2_TryLLMPrescribe_LLMError(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	// Mock server that returns an error.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		Model:            "test-model",
		APIKey:           "test-key",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 100000,
	}, noopLog2)

	var logMessages []string
	logFn := func(_ string, msg string, args ...any) {
		logMessages = append(logMessages, fmt.Sprintf(msg, args...))
	}

	tu := New(pool, TunerConfig{}, nil, logFn, WithLLM(client, nil))

	rx := tu.tryLLMPrescribe(
		context.Background(),
		candidate{QueryID: 1, Query: "SELECT 1"},
		[]PlanSymptom{{Kind: SymptomDiskSort}},
		"",
	)

	// Should return nil on LLM error.
	if rx != nil {
		t.Errorf("expected nil on LLM error, got %v", rx)
	}
	// Should have logged the error.
	found := false
	for _, msg := range logMessages {
		if strings.Contains(msg, "deterministic") ||
			strings.Contains(msg, "failed") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected error to be logged")
	}
}

func TestPhase2_TryLLMPrescribe_FallbackClient(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	// Primary fails, fallback succeeds.
	failSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	))
	defer failSrv.Close()

	mockResp := `{
		"choices": [{
			"message": {
				"content": "[{\"hint_directive\": \"Set(work_mem \\\"128MB\\\")\", \"rationale\": \"fallback hint\", \"confidence\": 0.7}]"
			}
		}],
		"usage": {"total_tokens": 50}
	}`
	okSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, mockResp)
		},
	))
	defer okSrv.Close()

	primary := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         failSrv.URL,
		Model:            "primary",
		APIKey:           "k",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 100000,
	}, noopLog2)

	fallback := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         okSrv.URL,
		Model:            "fallback",
		APIKey:           "k",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
	}, noopLog2)

	tu := New(
		pool, TunerConfig{}, nil, noopLog2,
		WithLLM(primary, fallback),
	)

	rx := tu.tryLLMPrescribe(
		context.Background(),
		candidate{
			QueryID:  2,
			Query:    "SELECT * FROM users",
			Calls:    50,
			MeanExecTime: 200.0,
		},
		[]PlanSymptom{{Kind: SymptomHashSpill}, {Kind: SymptomDiskSort}},
		"",
	)

	if len(rx) == 0 {
		t.Fatal("expected prescriptions from fallback LLM")
	}
}

func TestPhase2_TryLLMPrescribe_MalformedJSON(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	// Mock returns malformed JSON in the hint.
	mockResp := `{
		"choices": [{
			"message": {
				"content": "This is not JSON at all"
			}
		}],
		"usage": {"total_tokens": 10}
	}`
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, mockResp)
		},
	))
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		Model:            "test",
		APIKey:           "k",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 100000,
	}, noopLog2)

	tu := New(pool, TunerConfig{}, nil, noopLog2, WithLLM(client, nil))

	rx := tu.tryLLMPrescribe(
		context.Background(),
		candidate{QueryID: 1, Query: "SELECT 1"},
		nil, "",
	)

	// Should return nil on parse error.
	if rx != nil {
		t.Errorf("expected nil on malformed JSON, got %v", rx)
	}
}

// ---------------------------------------------------------------------------
// scanPlanForQuery (was 37.5%) — test with seeded explain_cache
// ---------------------------------------------------------------------------

func TestPhase2_ScanPlanForQuery_NoCache(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	tu := New(pool, TunerConfig{}, nil, noopLog2)
	symptoms := tu.scanPlanForQuery(context.Background(), 999999)
	if len(symptoms) != 0 {
		t.Errorf("expected 0 symptoms without cache, got %d",
			len(symptoms))
	}
}

func TestPhase2_ScanPlanForQuery_WithSeededCache(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	// Seed a plan with a disk sort symptom.
	diskSortPlan := []map[string]any{
		{
			"Plan": map[string]any{
				"Node Type":       "Sort",
				"Sort Space Used": int64(5000),
				"Sort Space Type": "Disk",
				"Plan Rows":       1000,
			},
		},
	}
	planJSON, _ := json.Marshal(diskSortPlan)

	testQueryID := int64(time.Now().UnixNano() % 1000000000)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sage.explain_cache (queryid, plan_json, source)
		 VALUES ($1, $2, 'test')`,
		testQueryID, planJSON,
	)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			t.Skipf("sage.explain_cache not available: %v", err)
		}
		t.Fatalf("insert plan: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.explain_cache WHERE queryid = $1`,
			testQueryID)
	}()

	tu := New(pool, TunerConfig{}, nil, noopLog2)
	symptoms := tu.scanPlanForQuery(
		context.Background(), testQueryID,
	)

	if len(symptoms) == 0 {
		t.Error("expected symptoms from disk sort plan")
	}

	found := false
	for _, s := range symptoms {
		if s.Kind == SymptomDiskSort {
			found = true
		}
	}
	if !found {
		t.Error("expected SymptomDiskSort in symptoms")
	}
}

func TestPhase2_ScanPlanForQuery_InvalidPlanJSON(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	testQueryID := int64(time.Now().UnixNano()%1000000000) + 1
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sage.explain_cache (queryid, plan_json, source)
		 VALUES ($1, $2, 'test')`,
		testQueryID, `{"not": "a plan"}`,
	)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			t.Skipf("sage.explain_cache not available: %v", err)
		}
		t.Fatalf("insert: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.explain_cache WHERE queryid = $1`,
			testQueryID)
	}()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, fmt.Sprintf(msg, args...))
	}

	tu := New(pool, TunerConfig{}, nil, logFn)
	symptoms := tu.scanPlanForQuery(
		context.Background(), testQueryID,
	)

	// Invalid plan JSON should produce 0 symptoms (not panic).
	if len(symptoms) != 0 {
		t.Errorf("expected 0 symptoms for invalid plan, got %d",
			len(symptoms))
	}
}

// ---------------------------------------------------------------------------
// fetchPlanJSON
// ---------------------------------------------------------------------------

func TestPhase2_FetchPlanJSON_Missing(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()

	tu := New(pool, TunerConfig{}, nil, noopLog2)
	got := tu.fetchPlanJSON(context.Background(), 888888888)
	if got != "" {
		t.Errorf("expected empty for missing queryid, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// isHighPlanTime
// ---------------------------------------------------------------------------

func TestPhase2_IsHighPlanTime_True(t *testing.T) {
	tu := New(nil, TunerConfig{
		PlanTimeRatio: 0.5,
		MinQueryCalls: 10,
	}, nil, noopLog2)

	c := candidate{
		MeanPlanTime: 100.0,
		MeanExecTime: 50.0,
		Calls:        20,
	}
	if !tu.isHighPlanTime(c) {
		t.Error("expected true: plan time 100 > exec 50 * 0.5")
	}
}

func TestPhase2_IsHighPlanTime_False_LowPlan(t *testing.T) {
	tu := New(nil, TunerConfig{
		PlanTimeRatio: 0.5,
		MinQueryCalls: 10,
	}, nil, noopLog2)

	c := candidate{
		MeanPlanTime: 10.0,
		MeanExecTime: 100.0,
		Calls:        20,
	}
	if tu.isHighPlanTime(c) {
		t.Error("expected false: plan time 10 < exec 100 * 0.5")
	}
}

func TestPhase2_IsHighPlanTime_False_LowCalls(t *testing.T) {
	tu := New(nil, TunerConfig{
		PlanTimeRatio: 0.5,
		MinQueryCalls: 100,
	}, nil, noopLog2)

	c := candidate{
		MeanPlanTime: 100.0,
		MeanExecTime: 50.0,
		Calls:        5, // below MinQueryCalls
	}
	if tu.isHighPlanTime(c) {
		t.Error("expected false: calls 5 < MinQueryCalls 100")
	}
}

func TestPhase2_IsHighPlanTime_ZeroPlanTime(t *testing.T) {
	tu := New(nil, TunerConfig{
		PlanTimeRatio: 0.5,
		MinQueryCalls: 1,
	}, nil, noopLog2)

	c := candidate{MeanPlanTime: 0, MeanExecTime: 100.0, Calls: 10}
	if tu.isHighPlanTime(c) {
		t.Error("expected false: zero plan time")
	}
}

// ---------------------------------------------------------------------------
// cooldownCycles
// ---------------------------------------------------------------------------

func TestPhase2_CooldownCycles_Default(t *testing.T) {
	tu := New(nil, TunerConfig{
		CascadeCooldownCycles: 0,
	}, nil, noopLog2)
	if tu.cooldownCycles() != 3 {
		t.Errorf("expected default 3, got %d", tu.cooldownCycles())
	}
}

func TestPhase2_CooldownCycles_Custom(t *testing.T) {
	tu := New(nil, TunerConfig{
		CascadeCooldownCycles: 7,
	}, nil, noopLog2)
	if tu.cooldownCycles() != 7 {
		t.Errorf("expected 7, got %d", tu.cooldownCycles())
	}
}

// ---------------------------------------------------------------------------
// parseLLMPrescriptions — edge cases
// ---------------------------------------------------------------------------

func TestPhase2_ParseLLMPrescriptions_EmptyArray(t *testing.T) {
	recs, err := parseLLMPrescriptions("[]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 recs, got %d", len(recs))
	}
}

func TestPhase2_ParseLLMPrescriptions_MarkdownWrapped(t *testing.T) {
	input := "```json\n" +
		`[{"hint_directive":"Set(work_mem \"64MB\")","rationale":"test","confidence":0.5}]` +
		"\n```"
	recs, err := parseLLMPrescriptions(input)
	if err != nil {
		t.Fatalf("markdown-wrapped should parse: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("expected 1 rec, got %d", len(recs))
	}
}

func TestPhase2_ParseLLMPrescriptions_WithThinkingText(
	t *testing.T,
) {
	input := "Let me analyze this query...\n\n" +
		`[{"hint_directive":"HashJoin(t1 t2)","rationale":"better join","confidence":0.8}]` +
		"\n\nHope this helps!"
	recs, err := parseLLMPrescriptions(input)
	if err != nil {
		t.Fatalf("should handle thinking text: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("expected 1 rec, got %d", len(recs))
	}
}

func TestPhase2_ParseLLMPrescriptions_EmptyString(t *testing.T) {
	recs, err := parseLLMPrescriptions("")
	if err != nil {
		t.Fatalf("empty string should not error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 recs, got %d", len(recs))
	}
}

func TestPhase2_ParseLLMPrescriptions_InvalidJSON(t *testing.T) {
	_, err := parseLLMPrescriptions("[{invalid json}]")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// validateHintSyntax — edge cases
// ---------------------------------------------------------------------------

func TestPhase2_ValidateHintSyntax_Valid(t *testing.T) {
	tests := []struct {
		hint string
		want bool
	}{
		{`Set(work_mem "256MB")`, true},
		{`HashJoin(t1 t2)`, true},
		{`Set(work_mem "256MB") HashJoin(t1 t2)`, true},
		{`IndexScan(orders idx_orders_status)`, true},
		{`NoSeqScan(orders)`, true},
		{`Parallel(orders 4)`, true},
		{"", false},
		{"DROP TABLE users", false},
		{"SET work_mem = '256MB'; DROP TABLE users", false},
		{"InvalidDirective(t1)", false},
	}
	for _, tt := range tests {
		got := validateHintSyntax(tt.hint)
		if got != tt.want {
			t.Errorf("validateHintSyntax(%q) = %v, want %v",
				tt.hint, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// convertPrescriptions — filters invalid hints
// ---------------------------------------------------------------------------

func TestPhase2_ConvertPrescriptions_FiltersInvalid(t *testing.T) {
	recs := []LLMPrescription{
		{
			HintDirective: `Set(work_mem "128MB")`,
			Rationale:     "good hint",
			Confidence:    0.8,
		},
		{
			HintDirective: "DROP TABLE users; --",
			Rationale:     "malicious",
			Confidence:    0.9,
		},
		{
			HintDirective: "",
			Rationale:     "empty",
			Confidence:    0.5,
		},
	}

	out := convertPrescriptions(recs, noopLog2)
	if len(out) != 1 {
		t.Fatalf("expected 1 valid prescription, got %d", len(out))
	}
	if !strings.Contains(out[0].HintDirective, "work_mem") {
		t.Errorf("wrong prescription kept: %s",
			out[0].HintDirective)
	}
}

// ---------------------------------------------------------------------------
// ScanPlan — plan parsing edge cases
// ---------------------------------------------------------------------------

func TestPhase2_ScanPlan_HashSpill(t *testing.T) {
	plan := `[{"Plan":{
		"Node Type":"Hash Join",
		"Hash Batches": 8,
		"Original Hash Batches": 1,
		"Peak Memory Usage": 4096,
		"Plan Rows": 1000,
		"Plans":[]
	}}]`
	symptoms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	found := false
	for _, s := range symptoms {
		if s.Kind == SymptomHashSpill {
			found = true
		}
	}
	if !found {
		t.Error("expected SymptomHashSpill for hash batches > 1")
	}
}

func TestPhase2_ScanPlan_SeqScan(t *testing.T) {
	plan := `[{"Plan":{
		"Node Type":"Seq Scan",
		"Relation Name":"orders",
		"Schema":"public",
		"Alias":"orders",
		"Plan Rows": 10000,
		"Plans":[]
	}}]`
	symptoms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	found := false
	for _, s := range symptoms {
		if s.Kind == SymptomSeqScanWithIndex {
			found = true
			if s.RelationName != "orders" {
				t.Errorf("relation: got %q", s.RelationName)
			}
		}
	}
	if !found {
		t.Error("expected SymptomSeqScanWithIndex")
	}
}

func TestPhase2_ScanPlan_EmptyPlan(t *testing.T) {
	symptoms, err := ScanPlan([]byte(`[{"Plan":{}}]`))
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	if len(symptoms) != 0 {
		t.Errorf("expected 0 symptoms for empty plan, got %d",
			len(symptoms))
	}
}

func TestPhase2_ScanPlan_BadNestedLoop(t *testing.T) {
	actual := int64(10000)
	plan := `[{"Plan":{
		"Node Type":"Nested Loop",
		"Plan Rows": 10,
		"Actual Rows": 10000,
		"Actual Loops": 1,
		"Plans":[]
	}}]`
	symptoms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	found := false
	for _, s := range symptoms {
		if s.Kind == SymptomBadNestedLoop {
			found = true
			ar, ok := s.Detail["actual_rows"]
			if !ok {
				t.Error("missing actual_rows in detail")
			} else if ar != actual {
				t.Errorf("actual_rows: got %v", ar)
			}
		}
	}
	if !found {
		t.Error("expected SymptomBadNestedLoop " +
			"(actual 10000 > plan 10 * 10)")
	}
}

// ---------------------------------------------------------------------------
// extractTables
// ---------------------------------------------------------------------------

func TestPhase2_ExtractTables(t *testing.T) {
	tests := []struct {
		query string
		want  int
	}{
		{"SELECT * FROM orders", 1},
		{"SELECT * FROM orders JOIN users ON true", 2},
		{"UPDATE orders SET status = 'done'", 1},
		{"INSERT INTO logs SELECT * FROM events", 2},
		{"SELECT 1", 0},
		{"SELECT * FROM pg_catalog.pg_class", 0}, // pg_ skipped
	}
	for _, tt := range tests {
		got := extractTables(tt.query)
		if len(got) != tt.want {
			t.Errorf("extractTables(%q) = %d tables, want %d",
				tt.query, len(got), tt.want)
		}
	}
}

func TestPhase2_ExtractTables_SchemaQualified(t *testing.T) {
	tables := extractTables("SELECT * FROM myschema.orders")
	if len(tables) != 1 {
		t.Fatalf("expected 1 table, got %d", len(tables))
	}
	if tables[0].schema != "myschema" {
		t.Errorf("schema: got %q", tables[0].schema)
	}
	if tables[0].name != "orders" {
		t.Errorf("name: got %q", tables[0].name)
	}
}

func TestPhase2_ExtractTables_SkipsSageSchema(t *testing.T) {
	tables := extractTables("SELECT * FROM sage.findings")
	if len(tables) != 0 {
		t.Errorf("sage schema should be skipped, got %d tables",
			len(tables))
	}
}
