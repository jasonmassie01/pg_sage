package advisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Roadmap 2.3: every advisor prompt carries the operator-confirmed facts
// of its database as bounded, fenced context.

type advisorFacts struct{ lines []string }

func (f advisorFacts) PromptLines(context.Context, ...string) []string { return f.lines }

func capturingAdvisorLLM(t *testing.T) (*llm.Manager, func() string) {
	t.Helper()
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "[]"},
				"finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 5}})
	}))
	t.Cleanup(srv.Close)
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 100000}, noopLog)
	return llm.NewManager(client, nil, false), func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestAdvisorPromptsCarryConfirmedFacts(t *testing.T) {
	mgr, last := capturingAdvisorLLM(t)
	a := &Advisor{logFn: noopLog}
	a.WithFacts(advisorFacts{lines: []string{
		"- fact #4: table app.audit_log is append-only (archive)."}})
	ctx := a.factsContext(context.Background())
	if _, err := chatAdvisor(ctx, mgr, "vacuum", "system", "", "table stats"); err != nil {
		t.Fatal(err)
	}
	body := last()
	if !strings.Contains(body, "fact #4: table app.audit_log is append-only") ||
		!strings.Contains(body, "Operator-confirmed facts") {
		t.Fatalf("request lacks the facts:\n%s", body)
	}
	if _, err := chatAdvisor(context.Background(), mgr, "vacuum", "system", "",
		"table stats again"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(last(), "Operator-confirmed facts") {
		t.Fatal("facts leaked into a call without them")
	}
}

func TestAdvisorWithoutFactsAddsNothing(t *testing.T) {
	a := &Advisor{logFn: noopLog}
	ctx := context.Background()
	if got := a.factsContext(ctx); got != ctx {
		t.Fatal("no facts source must leave the context unchanged")
	}
	a.WithFacts(advisorFacts{})
	if got := promptFactsFrom(a.factsContext(ctx)); got != "" {
		t.Fatalf("empty facts produced %q", got)
	}
}

// advisors-06: an over-long facts block is cut on a rune boundary, so the
// prompt never carries invalid UTF-8.
func TestFactsContext_RuneSafeCut(t *testing.T) {
	line := strings.Repeat("a", maxFactsChars-1) + "é" +
		strings.Repeat("b", 2000-maxFactsChars-1)
	if len(line) != 2000 || line[maxFactsChars-1] != 0xC3 {
		t.Fatalf("precondition: fixture is %d bytes", len(line))
	}
	a := &Advisor{logFn: noopLog}
	a.WithFacts(advisorFacts{lines: []string{line}})
	got := promptFactsFrom(a.factsContext(context.Background()))
	if got == "" || len(got) > maxFactsChars {
		t.Fatalf("facts block is %d bytes, want 1..%d", len(got), maxFactsChars)
	}
	if !utf8.ValidString(got) || !strings.HasPrefix(line, got) {
		t.Fatalf("facts block is not a valid UTF-8 prefix: %q", got[len(got)-4:])
	}
}

// Whole lines are kept when a line boundary fits under the cap.
func TestFactsContext_PrefersLineBoundary(t *testing.T) {
	first := "- fact #1: " + strings.Repeat("x", 900)
	second := "- fact #2: " + strings.Repeat("é", 400)
	a := &Advisor{logFn: noopLog}
	a.WithFacts(advisorFacts{lines: []string{first, second}})
	got := promptFactsFrom(a.factsContext(context.Background()))
	if got != first {
		t.Fatalf("facts block = %d bytes, want only the first whole line (%d)",
			len(got), len(first))
	}
}
