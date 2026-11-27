package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// No concurrent access tests: the truncation note is a pure function of
// one response, and the worker adds no shared state for it.

const cutNote = "\n\n_(cut at the model's token limit)_"

// finishLLM answers every request with content and the given finish reason.
func finishLLM(t *testing.T, content, finish string) *llm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content},
				"finish_reason": finish}},
			"usage": map[string]int{"total_tokens": 77},
		})
	}))
	t.Cleanup(srv.Close)
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, JSONMode: true}, noopLog)
}

// llm-07: a briefing cut at the token limit keeps the model's text byte for
// byte (no JSON "repair" of prose), says it was cut, and logs it.
func TestEnhanceWithLLM_TruncatedGetsNote(t *testing.T) {
	cut := "## Summary\nThe index [critical: {a} and"
	lc := &logCapture{}
	w := &Worker{cfg: &config.Config{}, llm: finishLLM(t, cut, "length"), logFn: lc.logFn}
	text, tokens, err := w.enhanceWithLLM(context.Background(), "data")
	if err != nil {
		t.Fatalf("enhanceWithLLM: %v", err)
	}
	if text != cut+cutNote || tokens != 77 {
		t.Fatalf("text=%q tokens=%d, want %q and 77", text, tokens, cut+cutNote)
	}
	if len(lc.messages) != 1 || !strings.Contains(lc.messages[0], "token limit") ||
		!strings.HasPrefix(lc.messages[0], "[WARN]") {
		t.Fatalf("truncation not logged once as WARN: %q", lc.messages)
	}
}

// State: finish_reason=stop adds no note and logs nothing.
func TestEnhanceWithLLM_CompleteHasNoNote(t *testing.T) {
	lc := &logCapture{}
	w := &Worker{cfg: &config.Config{}, llm: finishLLM(t, "All healthy.", "stop"),
		logFn: lc.logFn}
	text, _, err := w.enhanceWithLLM(context.Background(), "data")
	if err != nil {
		t.Fatalf("enhanceWithLLM: %v", err)
	}
	if text != "All healthy." || len(lc.messages) != 0 {
		t.Fatalf("text=%q logs=%q, want unchanged and silent", text, lc.messages)
	}
}

// Boundary: a cut inside a fenced block is closed first, so the note is
// prose that grounding keeps, not code it may strip.
func TestEnhanceWithLLM_TruncatedInsideFence(t *testing.T) {
	for _, tc := range []struct{ name, cut, closer string }{
		{"backticks", "Run:\n```sql\nCREATE INDEX", "\n```"},
		{"long tildes", "Run:\n~~~~\nVACUUM", "\n~~~~"},
		{"ends with newline", "Run:\n```\nVACUUM\n", "```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{cfg: &config.Config{}, llm: finishLLM(t, tc.cut, "length"),
				logFn: noopLog}
			text, _, err := w.enhanceWithLLM(context.Background(), "data")
			if err != nil {
				t.Fatalf("enhanceWithLLM: %v", err)
			}
			if want := tc.cut + tc.closer + cutNote; text != want {
				t.Fatalf("text = %q, want %q", text, want)
			}
			plain, _ := groundOutput(text, Payload{})
			if !strings.HasSuffix(plain, cutNote) {
				t.Fatalf("grounding dropped the note: %q", plain)
			}
		})
	}
}

// Nil/empty: a cut answer that is only whitespace stays an error (the
// structured briefing is used), never an empty briefing with a note.
func TestEnhanceWithLLM_EmptyTruncatedIsError(t *testing.T) {
	w := &Worker{cfg: &config.Config{}, llm: finishLLM(t, " \n", "length"), logFn: noopLog}
	text, tokens, err := w.enhanceWithLLM(context.Background(), "data")
	if err == nil || text != "" || tokens != 77 {
		t.Fatalf("got (%q, %d, %v), want an empty-response error with 77 tokens",
			text, tokens, err)
	}
}

// Error propagation, on real PostgreSQL: the truncated briefing is stored
// with the note, and the dispatched text carries it too.
func TestGenerateStoresTruncatedBriefingWithNote_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)
	id := seedGroundFinding(t, pool, ctx)
	cut := fmt.Sprintf("Finding #%d needs an index. The table is grow", id)
	out, err := New(pool, &config.Config{}, finishLLM(t, cut, "length"), noopLog).Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasSuffix(out, cutNote) || !strings.Contains(out, "The table is grow") {
		t.Fatalf("dispatched briefing lacks the note or the text:\n%s", out)
	}
	_, text := latestPayload(t, pool, ctx)
	if !strings.HasSuffix(text, cutNote) || !strings.Contains(text, "The table is grow") {
		t.Fatalf("stored briefing lacks the note or the text:\n%s", text)
	}
	var llmUsed bool
	var tokens int
	if err := pool.QueryRow(ctx, `SELECT llm_used, token_count FROM sage.briefings
		WHERE content_json::text LIKE '%llm08_missing_index%'
		ORDER BY id DESC LIMIT 1`).Scan(&llmUsed, &tokens); err != nil {
		t.Fatalf("read briefing row: %v", err)
	}
	if !llmUsed || tokens != 77 {
		t.Fatalf("llm_used=%v token_count=%d, want true and 77", llmUsed, tokens)
	}
}
