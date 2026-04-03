//go:build e2e

// Package e2e — LLM integration tests that hit the real Gemini
// API. These exercise the actual prompt+response parsing paths
// that mocks cannot cover.
//
// Run with:
//
//	SAGE_LLM_API_KEY=<key> go test -tags=e2e -count=1 \
//	    -timeout 300s ./e2e/ -run TestLLM
//
// Requires: SAGE_LLM_API_KEY set to a valid Gemini API key.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const (
	geminiEndpoint = "https://generativelanguage.googleapis.com" +
		"/v1beta/openai"
	geminiModel  = "gemini-2.5-flash"
	llmTestTTL   = 30 * time.Second
	smallBudget  = 1000
	largeBudget  = 1_000_000
)

// requireAPIKey skips the test if SAGE_LLM_API_KEY is unset.
func requireAPIKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("SAGE_LLM_API_KEY")
	if key == "" {
		t.Skip("SAGE_LLM_API_KEY not set, " +
			"skipping live LLM test")
	}
	return key
}

// newTestLLMConfig builds an LLMConfig for the Gemini endpoint
// with the given token budget.
func newTestLLMConfig(
	apiKey string, budget int,
) *config.LLMConfig {
	return &config.LLMConfig{
		Enabled:          true,
		Endpoint:         geminiEndpoint,
		APIKey:           apiKey,
		Model:            geminiModel,
		TimeoutSeconds:   30,
		TokenBudgetDaily: budget,
		CooldownSeconds:  5,
	}
}

// testLogFn returns a log function that forwards to t.Logf.
func testLogFn(t *testing.T) func(string, string, ...any) {
	t.Helper()
	return func(
		component string, format string, args ...any,
	) {
		t.Helper()
		t.Logf("[%s] "+format, append(
			[]any{component}, args...,
		)...)
	}
}

// TestLLMBasicChat verifies that the llm.Client can send a
// simple prompt to Gemini and receive a non-empty response
// with a token count > 0.
func TestLLMBasicChat(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, largeBudget)
	client := llm.New(cfg, testLogFn(t))

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	resp, tokens, err := client.Chat(
		ctx,
		"You are a helpful assistant.",
		"What is PostgreSQL? Answer in one sentence.",
		256,
	)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	t.Logf("Response (%d tokens): %s", tokens, resp)

	if resp == "" {
		t.Fatal("expected non-empty response")
	}
	if tokens <= 0 {
		t.Errorf("expected tokens > 0, got %d", tokens)
	}
	lower := strings.ToLower(resp)
	if !strings.Contains(lower, "database") &&
		!strings.Contains(lower, "sql") &&
		!strings.Contains(lower, "postgres") {
		t.Errorf(
			"response does not mention database/sql/postgres: %s",
			resp,
		)
	}
}

// TestLLMBriefingGeneration sends a prompt that mirrors what
// the briefing package sends, using fake snapshot data.
// Verifies the response is non-empty text.
func TestLLMBriefingGeneration(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, largeBudget)
	client := llm.New(cfg, testLogFn(t))

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	system := `You are a PostgreSQL DBA assistant. ` +
		`Generate a concise health briefing based on ` +
		`the database statistics provided. ` +
		`Use plain text, not JSON.`

	user := `Database: production
Active connections: 45 / 100
Cache hit ratio: 98.7%
Dead tuples: 12,340 across 5 tables
Largest table: orders (2.3 GB, 15M rows)
Slow queries (>500ms): 3 in last hour
Unused indexes: 7
Sequence "orders_id_seq": 42% exhausted
Replication lag: 1.2 seconds

Summarize the health status and highlight any concerns.`

	resp, tokens, err := client.Chat(
		ctx, system, user, 1024,
	)
	if err != nil {
		t.Fatalf("Briefing Chat failed: %v", err)
	}

	t.Logf("Briefing (%d tokens):\n%s", tokens, resp)

	if resp == "" {
		t.Fatal("expected non-empty briefing response")
	}
	if tokens <= 0 {
		t.Errorf("expected tokens > 0, got %d", tokens)
	}
	// The briefing should mention at least one concern.
	lower := strings.ToLower(resp)
	hasConcern := strings.Contains(lower, "dead tuple") ||
		strings.Contains(lower, "unused index") ||
		strings.Contains(lower, "sequence") ||
		strings.Contains(lower, "replication") ||
		strings.Contains(lower, "slow quer")
	if !hasConcern {
		t.Errorf(
			"briefing does not reference any known concern",
		)
	}
}

// TestLLMOptimizerIndexRecommendation sends a prompt that
// mirrors the optimizer's index recommendation request with
// realistic pg_stat data. Verifies the response parses as
// valid JSON with expected fields.
func TestLLMOptimizerIndexRecommendation(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, largeBudget)
	client := llm.New(cfg, testLogFn(t))

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	system := `CRITICAL: Respond with ONLY a JSON array. ` +
		`No thinking, no reasoning, no explanation, ` +
		`no markdown fences. Start with [ and end with ].

You are a PostgreSQL index optimization expert. ` +
		`Analyze table structure and query patterns ` +
		`to recommend indexes.

Output format:
[
  {
    "table": "schema.table",
    "ddl": "CREATE INDEX CONCURRENTLY ...",
    "rationale": "Why this index helps",
    "index_type": "btree|gin|gist|brin|hash",
    "estimated_improvement_pct": 25.0
  }
]

If no indexes needed, return: []`

	user := `## Table: public.orders
Live tuples: 15000000 | Dead tuples: 12340
Table size: 2.3 GB | Index size: 890.2 MB
Write rate: 22.5% | Workload: read-heavy
Existing indexes: 2 | Collation: en_US.UTF-8

### Columns
- id bigint
- customer_id bigint (nullable)
- status text
- created_at timestamptz
- total_amount numeric(10,2)
- shipping_address jsonb (nullable)

### Existing Indexes
- orders_pkey [UNIQUE]: btree (id) (scans: 9500000)
- idx_orders_created: btree (created_at) (scans: 120000)

### Queries
- [calls=450000, mean=12.50ms, total=5625000.00ms] ` +
		`SELECT * FROM orders WHERE customer_id = $1 ` +
		`ORDER BY created_at DESC LIMIT 50
- [calls=85000, mean=45.20ms, total=3842000.00ms] ` +
		`SELECT * FROM orders WHERE status = $1 ` +
		`AND created_at > $2
- [calls=12000, mean=120.00ms, total=1440000.00ms] ` +
		`SELECT customer_id, SUM(total_amount) ` +
		`FROM orders WHERE created_at > $1 ` +
		`GROUP BY customer_id

RESPOND NOW with ONLY the JSON array. Start with [.`

	resp, tokens, err := client.Chat(
		ctx, system, user, 4096,
	)
	if err != nil {
		t.Fatalf("Optimizer Chat failed: %v", err)
	}

	t.Logf("Optimizer (%d tokens):\n%s", tokens, resp)

	// Strip markdown fences or thinking tokens if present.
	cleaned := extractJSONArray(resp)
	if cleaned == "" {
		t.Fatalf(
			"could not extract JSON array from response: %s",
			resp,
		)
	}

	var recs []map[string]any
	if err := json.Unmarshal(
		[]byte(cleaned), &recs,
	); err != nil {
		t.Fatalf(
			"JSON parse failed: %v\ncleaned: %s", err, cleaned,
		)
	}

	t.Logf("Parsed %d recommendations", len(recs))

	if len(recs) == 0 {
		t.Fatal("expected at least one index recommendation")
	}

	// Verify expected fields in the first recommendation.
	first := recs[0]
	for _, field := range []string{
		"table", "ddl", "rationale",
	} {
		val, ok := first[field]
		if !ok {
			t.Errorf("missing field %q in recommendation", field)
			continue
		}
		s, isStr := val.(string)
		if !isStr || s == "" {
			t.Errorf("field %q is empty or not a string", field)
		}
	}

	// DDL should reference CREATE INDEX.
	ddl, _ := first["ddl"].(string)
	upper := strings.ToUpper(ddl)
	if !strings.Contains(upper, "CREATE INDEX") {
		t.Errorf(
			"ddl does not contain CREATE INDEX: %s", ddl,
		)
	}
}

// TestLLMAdvisorVacuumRecommendation sends a prompt mirroring
// the advisor's vacuum tuning request. Verifies the JSON
// response parses correctly.
func TestLLMAdvisorVacuumRecommendation(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, largeBudget)
	client := llm.New(cfg, testLogFn(t))

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	system := `CRITICAL: Respond with ONLY a JSON array. ` +
		`No thinking, no reasoning, no markdown fences. ` +
		`Start with [ and end with ].

You are a PostgreSQL vacuum tuning expert. ` +
		`Analyze table statistics and recommend ` +
		`vacuum parameter changes.

Output format:
[
  {
    "object_identifier": "schema.table",
    "severity": "info|warning|critical",
    "rationale": "Why this change helps",
    "recommended_sql": "ALTER TABLE ... SET (...)"
  }
]

If no changes needed, return: []`

	user := `Database: production (PostgreSQL 16.2)
autovacuum_vacuum_scale_factor: 0.2
autovacuum_analyze_scale_factor: 0.1
autovacuum_vacuum_cost_delay: 2ms

Table statistics:
- public.events: 50M rows, 2.1M dead tuples (4.2%), ` +
		`last vacuum 3 days ago
- public.sessions: 12M rows, 890K dead tuples (7.4%), ` +
		`last vacuum 12 hours ago
- public.audit_log: 200M rows, 15M dead tuples (7.5%), ` +
		`last vacuum 7 days ago, append-only workload

Recommend vacuum parameter tuning for these tables.
RESPOND NOW with ONLY the JSON array. Start with [.`

	resp, tokens, err := client.Chat(
		ctx, system, user, 4096,
	)
	if err != nil {
		t.Fatalf("Advisor Chat failed: %v", err)
	}

	t.Logf("Advisor (%d tokens):\n%s", tokens, resp)

	cleaned := extractJSONArray(resp)
	if cleaned == "" {
		t.Fatalf(
			"could not extract JSON array from response: %s",
			resp,
		)
	}

	var recs []map[string]any
	if err := json.Unmarshal(
		[]byte(cleaned), &recs,
	); err != nil {
		t.Fatalf(
			"JSON parse failed: %v\ncleaned: %s", err, cleaned,
		)
	}

	t.Logf("Parsed %d vacuum recommendations", len(recs))

	if len(recs) == 0 {
		t.Fatal(
			"expected at least one vacuum recommendation",
		)
	}

	// Verify fields.
	first := recs[0]
	for _, field := range []string{
		"object_identifier", "rationale",
	} {
		val, ok := first[field]
		if !ok {
			t.Errorf("missing field %q", field)
			continue
		}
		s, isStr := val.(string)
		if !isStr || s == "" {
			t.Errorf("field %q is empty or not a string", field)
		}
	}
}

// TestLLMMarkdownWrappedJSON verifies that the
// RepairTruncatedJSON and stripToJSON-style extraction handle
// responses wrapped in ```json fences, which Gemini often
// produces despite explicit instructions not to.
func TestLLMMarkdownWrappedJSON(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, largeBudget)
	client := llm.New(cfg, testLogFn(t))

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	// Deliberately ask for JSON in a way that may trigger
	// markdown wrapping.
	system := "You are a helpful assistant."
	user := `Return a JSON array with exactly 2 objects. ` +
		`Each object should have fields "name" (string) ` +
		`and "value" (number). ` +
		`Example: [{"name":"a","value":1}]. ` +
		`Return ONLY the JSON array, nothing else.`

	resp, tokens, err := client.Chat(
		ctx, system, user, 512,
	)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	t.Logf("Raw response (%d tokens): %s", tokens, resp)

	// Try extracting JSON regardless of wrapping.
	cleaned := extractJSONArray(resp)
	if cleaned == "" {
		t.Fatalf(
			"could not extract JSON from response: %s", resp,
		)
	}

	var items []map[string]any
	if err := json.Unmarshal(
		[]byte(cleaned), &items,
	); err != nil {
		// If normal extraction failed, try RepairTruncatedJSON.
		repaired := llm.RepairTruncatedJSON(resp)
		if err2 := json.Unmarshal(
			[]byte(repaired), &items,
		); err2 != nil {
			t.Fatalf(
				"JSON parse failed even after repair: %v\n"+
					"cleaned: %s\nrepaired: %s",
				err2, cleaned, repaired,
			)
		}
		t.Logf("RepairTruncatedJSON salvaged the response")
	}

	if len(items) < 1 {
		t.Fatal("expected at least 1 JSON object in array")
	}

	// Verify structure.
	for i, item := range items {
		if _, ok := item["name"]; !ok {
			t.Errorf("item[%d] missing 'name' field", i)
		}
		if _, ok := item["value"]; !ok {
			t.Errorf("item[%d] missing 'value' field", i)
		}
	}
}

// TestLLMTokenBudgetTracking creates a client with a small
// token budget, makes one call, and verifies that
// TokensUsedToday() increases.
func TestLLMTokenBudgetTracking(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, smallBudget)
	client := llm.New(cfg, testLogFn(t))

	before := client.TokensUsedToday()
	if before != 0 {
		t.Fatalf(
			"expected 0 tokens used initially, got %d",
			before,
		)
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	resp, tokens, err := client.Chat(
		ctx,
		"You are a helpful assistant.",
		"Say hello in exactly 3 words.",
		64,
	)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	t.Logf(
		"Response (%d tokens): %s", tokens, resp,
	)

	after := client.TokensUsedToday()
	if after <= before {
		t.Errorf(
			"tokensUsedToday did not increase: "+
				"before=%d after=%d",
			before, after,
		)
	}
	t.Logf("Tokens used: %d -> %d", before, after)

	// With a budget of 1000, a second big call should
	// eventually exhaust it. Make a call that will push
	// us over if we haven't already.
	if after < int64(smallBudget) {
		_, _, err2 := client.Chat(
			ctx,
			"You are a helpful assistant.",
			"Write a 500-word essay about PostgreSQL MVCC.",
			512,
		)
		// The call may succeed (if tokens used < budget)
		// or fail with budget exhausted. Either is valid.
		if err2 != nil {
			if !strings.Contains(
				err2.Error(), "budget exhausted",
			) {
				t.Fatalf(
					"expected budget error, got: %v", err2,
				)
			}
			t.Logf("Budget correctly exhausted: %v", err2)
		} else {
			final := client.TokensUsedToday()
			t.Logf("Tokens after 2nd call: %d", final)
		}
	}
}

// TestLLMCircuitBreakerBadEndpoint creates a client pointing
// at a bad URL, makes 3 calls, and verifies the circuit
// breaker opens.
func TestLLMCircuitBreakerBadEndpoint(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/bad",
		APIKey:           "fake-key-for-circuit-test",
		Model:            "test-model",
		TimeoutSeconds:   3,
		TokenBudgetDaily: largeBudget,
		CooldownSeconds:  60,
	}
	client := llm.New(cfg, testLogFn(t))

	ctx, cancel := context.WithTimeout(
		context.Background(), llmTestTTL,
	)
	defer cancel()

	// Make 3 failing calls to trip the circuit breaker.
	for i := 0; i < 3; i++ {
		_, _, err := client.Chat(
			ctx,
			"system",
			"user prompt",
			64,
		)
		if err == nil {
			t.Fatalf(
				"call %d: expected error from bad endpoint",
				i+1,
			)
		}
		t.Logf("Call %d error (expected): %v", i+1, err)
	}

	// The 4th call should fail immediately with circuit
	// breaker open.
	if !client.IsCircuitOpen() {
		t.Fatal(
			"circuit breaker should be open after 3 failures",
		)
	}

	_, _, err := client.Chat(
		ctx,
		"system",
		"should not reach API",
		64,
	)
	if err == nil {
		t.Fatal("expected circuit breaker error")
	}
	if !strings.Contains(err.Error(), "circuit breaker") {
		t.Errorf(
			"expected 'circuit breaker' in error, got: %v",
			err,
		)
	}
	t.Logf("Circuit breaker correctly open: %v", err)
}

// extractJSONArray extracts a JSON array from a response that
// may be wrapped in markdown fences or contain thinking tokens.
// This mirrors the stripToJSON logic used by optimizer/advisor.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	// Find the first [ and last ] to extract the array.
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	// Try stripping markdown fences.
	s = stripFences(s)
	start = strings.Index(s, "[")
	end = strings.LastIndex(s, "]")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return ""
}

// stripFences removes ```json ... ``` wrappers.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
