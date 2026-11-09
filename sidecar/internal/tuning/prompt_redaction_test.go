package tuning

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// llm-09 CHECK-22: statement text that reaches the tuning model has no
// number of 6 or more digits left in a comparison; short numbers stay.

// comparisonNumber is a 6+ digit literal right after a comparison operator
// or anywhere in an IN (...) list.
var comparisonNumber = regexp.MustCompile(`(=|<>|!=|<|>) *-?\d{6,}|(?i:\bIN) *\([^()]*\b\d{6,}`)

func TestLLM_PromptRedactsLongNumbersInComparisons(t *testing.T) {
	h := newHarness(t)
	srv := newFakeOpenAI(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			completion(w, "", wireToolCall("statement", `{"queryid":101}`))
			return
		}
		completion(w, fencedIndexAnswer, nil)
	})
	h.agent = llmAgent(t, srv.srv.URL, 10, h)
	prev, cur := ordersPair()
	const pii = "SELECT * FROM public.orders WHERE customer_id = 4111111111111111 " +
		"AND id IN (123456, 7) LIMIT 10"
	prev.Queries[0].Query, cur.Queries[0].Query = pii, pii
	if _, err := h.agent.Tune(context.Background(), cur, prev); err != nil {
		t.Fatalf("tune: %v", err)
	}
	if srv.calls.Load() < 2 {
		t.Fatalf("model called %d times, want the tool turn and the answer", srv.calls.Load())
	}
	var all strings.Builder
	for i := 0; i < int(srv.calls.Load()); i++ {
		msgs, _ := srv.body(i)["messages"].([]any)
		for _, m := range msgs {
			content, _ := m.(map[string]any)["content"].(string)
			all.WriteString(content + "\n")
		}
	}
	prompt := all.String()
	if strings.Contains(prompt, "4111111111111111") || strings.Contains(prompt, "123456") {
		t.Fatalf("a long number reached the model:\n%s", prompt)
	}
	if m := comparisonNumber.FindString(prompt); m != "" {
		t.Fatalf("6+ digit comparison literal %q reached the model:\n%s", m, prompt)
	}
	if !strings.Contains(prompt, "customer_id = ?") || !strings.Contains(prompt, "IN (?, 7)") {
		t.Fatalf("redacted statement not in the prompt:\n%s", prompt)
	}
}
