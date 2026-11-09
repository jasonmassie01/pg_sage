package advisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-05: llm.context_budget_tokens is the trim target for attached
// context. The advisor's data trims to min(16384 bytes, budget × 4); a
// budget of 0 means no extra trim.

func TestAdvisorPromptLimit(t *testing.T) {
	for _, tc := range []struct{ budget, want int }{
		{0, maxAdvisorPromptChars},
		{-1, maxAdvisorPromptChars},
		{8192, 8192},
		{maxAdvisorPromptChars, maxAdvisorPromptChars},
		{maxAdvisorPromptChars + 1, maxAdvisorPromptChars},
		{32768, maxAdvisorPromptChars},
		{4, 4},
	} {
		if got := advisorPromptLimit(tc.budget); got != tc.want {
			t.Errorf("advisorPromptLimit(%d) = %d, want %d", tc.budget, got, tc.want)
		}
	}
}

func TestTruncateAdvisorPrompt_BudgetOnRuneBoundary(t *testing.T) {
	long := "x" + strings.Repeat("é", 20000) // odd byte offset: cuts land mid-rune
	got := truncateAdvisorPrompt(long, 8192)
	if !utf8.ValidString(got) {
		t.Fatal("budget cut split a rune")
	}
	if len(got) > 8192 || len(got) < 8190 {
		t.Fatalf("len = %d, want just under 8192", len(got))
	}
	if got = truncateAdvisorPrompt(long, 0); len(got) > maxAdvisorPromptChars ||
		len(got) < maxAdvisorPromptChars-2 {
		t.Fatalf("budget 0: len = %d, want just under %d", len(got), maxAdvisorPromptChars)
	}
	if got := truncateAdvisorPrompt("", 8192); got != "" {
		t.Fatalf("empty prompt = %q", got)
	}
	if got := truncateAdvisorPrompt("short", 4); got != "shor" {
		t.Fatalf("tiny budget = %q, want shor", got)
	}
}

// wireUser captures the user message of each advisor request.
func wireUser(t *testing.T, got *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		for _, m := range req.Messages {
			if m.Role == "user" {
				*got = append(*got, m.Content)
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[]"},` +
			`"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatAdvisor_TrimsDataToContextBudget(t *testing.T) {
	data := strings.Repeat("Table: public.t\n  Dead tuples: 5000\n\n", 2000)
	for _, tc := range []struct {
		budgetTokens, wantMax int
	}{
		{2048, 8192},
		{0, maxAdvisorPromptChars},
		{config.DefaultLLMContextBudget, maxAdvisorPromptChars},
	} {
		var users []string
		srv := wireUser(t, &users)
		client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL,
			APIKey: "k", Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1000000,
			ContextBudgetTokens: tc.budgetTokens}, noopLog)
		mgr := llm.NewManager(client, nil, false)
		if _, err := chatAdvisor(context.Background(), mgr, "bloat", "Return JSON.",
			"", data); err != nil {
			t.Fatalf("budget %d: %v", tc.budgetTokens, err)
		}
		if len(users) != 1 {
			t.Fatalf("budget %d: %d requests", tc.budgetTokens, len(users))
		}
		body := dataBlockBody(t, users[0])
		if len(body) > tc.wantMax || len(body) < tc.wantMax/2 {
			t.Errorf("budget %d: data = %d bytes, want at most %d (and over half)",
				tc.budgetTokens, len(body), tc.wantMax)
		}
	}
}

// dataBlockBody returns the text inside the first <data> block.
func dataBlockBody(t *testing.T, user string) string {
	t.Helper()
	start := strings.Index(user, "\">\n")
	end := strings.Index(user, "\n</data>")
	if start < 0 || end < start {
		t.Fatalf("no data block in %q", user)
	}
	return user[start+3 : end]
}
