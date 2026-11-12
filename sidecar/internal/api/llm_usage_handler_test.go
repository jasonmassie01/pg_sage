package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-04: GET /api/v1/llm/usage?days=N returns the attributed usage ledger.
//
// No database here: the registry is a fake; the sidecar's reader is
// tested against PostgreSQL in internal/store and internal/llm.

type usageRegistry struct {
	*llm.Manager
	rows    []LLMUsageRow
	err     error
	gotDays int
}

func (r *usageRegistry) LLMUsage(_ context.Context, days int) ([]LLMUsageRow, error) {
	r.gotDays = days
	return r.rows, r.err
}

func getUsage(t *testing.T, registry LLMBudgetRegistry, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	llmUsageHandler(registry)(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/llm/usage"+query, nil))
	return w
}

func newUsageRegistry() *usageRegistry {
	return &usageRegistry{Manager: llm.NewManager(testLLMClient("m", 1000), nil, false)}
}

// Happy path: the rows come back as JSON, an unpriced row's cost null.
func TestLLMUsageHandler_ReturnsRows(t *testing.T) {
	cost := 0.0123
	reg := newUsageRegistry()
	reg.rows = []LLMUsageRow{
		{Day: "2026-11-12", DatabaseKey: "id:7", Database: "orders", LLMRole: "general",
			Purpose: "briefing", Model: "m", PromptTokens: 1200, CompletionTokens: 300,
			Calls: 1, EstCostUSD: &cost, Source: "provider"},
		{Day: "2026-11-12", DatabaseKey: "process", LLMRole: "optimizer",
			Purpose: "query_tuning", Model: "o", ReasoningTokens: 50, Calls: 2, Failures: 1,
			Source: "estimate"},
	}
	w := getUsage(t, reg, "?days=7")
	if w.Code != http.StatusOK || reg.gotDays != 7 {
		t.Fatalf("status %d, days %d: %s", w.Code, reg.gotDays, w.Body.String())
	}
	var body struct {
		Days int               `json:"days"`
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Days != 7 || len(body.Rows) != 2 {
		t.Fatalf("body = %s, want days 7 and 2 rows", w.Body.String())
	}
	first, second := string(body.Rows[0]), string(body.Rows[1])
	for _, want := range []string{`"purpose":"briefing"`, `"prompt_tokens":1200`,
		`"completion_tokens":300`, `"est_cost_usd":0.0123`, `"database":"orders"`,
		`"llm_role":"general"`, `"source":"provider"`} {
		if !strings.Contains(first, want) {
			t.Errorf("first row %s lacks %s", first, want)
		}
	}
	for _, want := range []string{`"est_cost_usd":null`, `"reasoning_tokens":50`,
		`"failures":1`, `"source":"estimate"`} {
		if !strings.Contains(second, want) {
			t.Errorf("second row %s lacks %s", second, want)
		}
	}
}

// Boundary: days defaults to 30 and accepts 1 to 400; anything else is a
// 400 naming the range.
func TestLLMUsageHandler_DaysBoundaries(t *testing.T) {
	for query, want := range map[string]int{"": 30, "?days=1": 1, "?days=400": 400} {
		reg := newUsageRegistry()
		if w := getUsage(t, reg, query); w.Code != http.StatusOK || reg.gotDays != want {
			t.Errorf("%q: status %d, days %d, want 200 and %d", query, w.Code, reg.gotDays, want)
		}
	}
	for _, query := range []string{"?days=0", "?days=-1", "?days=401", "?days=abc",
		"?days=1.5"} {
		reg := newUsageRegistry()
		w := getUsage(t, reg, query)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "1 and 400") {
			t.Errorf("%q: status %d %s, want 400 naming 1 and 400", query, w.Code,
				w.Body.String())
		}
		if reg.gotDays != 0 {
			t.Errorf("%q reached the reader with days %d", query, reg.gotDays)
		}
	}
}

// Nil/empty and error propagation: no registry, a registry without a
// ledger and an unavailable ledger are 503; a read failure is a 500 that
// does not echo the database error; no rows is an empty list, not null.
func TestLLMUsageHandler_UnavailableAndErrors(t *testing.T) {
	if w := getUsage(t, nil, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil registry status = %d, want 503", w.Code)
	}
	plain := llm.NewManager(testLLMClient("m", 1000), nil, false)
	if w := getUsage(t, plain, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("manager without a ledger status = %d, want 503", w.Code)
	}
	reg := newUsageRegistry()
	reg.err = ErrLLMUsageUnavailable
	if w := getUsage(t, reg, ""); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "control database") {
		t.Errorf("unavailable ledger = %d %s, want 503 naming the control database",
			w.Code, w.Body.String())
	}
	reg.err = errors.New(`pq: secret detail about sage.llm_usage`)
	if w := getUsage(t, reg, ""); w.Code != http.StatusInternalServerError ||
		strings.Contains(w.Body.String(), "secret detail") {
		t.Errorf("read failure = %d %s, want a 500 without the database error",
			w.Code, w.Body.String())
	}
	reg.err = nil
	w := getUsage(t, reg, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Errorf("no rows = %d %s, want 200 with an empty list", w.Code, w.Body.String())
	}
}
