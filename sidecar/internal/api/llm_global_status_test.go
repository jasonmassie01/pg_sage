package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-03: /api/v1/llm/status carries the process-wide ceiling as
// global{ceiling, used, source} when the registry enforces one.

type globalBudgetRegistry struct {
	*llm.Manager
	global llm.BudgetStatus
}

func (r globalBudgetRegistry) GlobalBudget() llm.BudgetStatus { return r.global }

func getLLMStatus(t *testing.T, registry LLMBudgetRegistry) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	llmStatusHandler(registry)(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/llm/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestLLMStatusHandler_ReportsGlobalCeiling(t *testing.T) {
	registry := globalBudgetRegistry{
		Manager: llm.NewManager(testLLMClient("m", 1000), nil, false),
		global: llm.BudgetStatus{Ceiling: 2500000, Used: 1200, Reserved: 300,
			Source: "derived", Exhausted: false},
	}
	body := getLLMStatus(t, registry)
	var global struct {
		Ceiling   int64  `json:"ceiling"`
		Used      int64  `json:"used"`
		Reserved  int64  `json:"reserved"`
		Source    string `json:"source"`
		Exhausted bool   `json:"exhausted"`
	}
	if err := json.Unmarshal(body["global"], &global); err != nil {
		t.Fatalf("global = %s: %v", body["global"], err)
	}
	if global.Ceiling != 2500000 || global.Used != 1200 || global.Reserved != 300 ||
		global.Source != "derived" || global.Exhausted {
		t.Fatalf("global = %+v", global)
	}
	if _, ok := body["clients"]; !ok {
		t.Fatal("clients must still be reported")
	}
}

// An exhausted global ceiling raises the aggregate banner flag.
func TestLLMStatusHandler_ExhaustedGlobalRaisesAnyExhausted(t *testing.T) {
	registry := globalBudgetRegistry{
		Manager: llm.NewManager(testLLMClient("m", 1000), nil, false),
		global:  llm.BudgetStatus{Ceiling: 100, Used: 100, Source: "configured", Exhausted: true},
	}
	body := getLLMStatus(t, registry)
	if string(body["any_exhausted"]) != "true" {
		t.Fatalf("any_exhausted = %s, want true", body["any_exhausted"])
	}
}

// A registry without a global ceiling omits the field.
func TestLLMStatusHandler_NoGlobalWithoutReporter(t *testing.T) {
	body := getLLMStatus(t, llm.NewManager(testLLMClient("m", 1000), nil, false))
	if _, ok := body["global"]; ok {
		t.Fatalf("global = %s, want absent for a plain manager", body["global"])
	}
}
