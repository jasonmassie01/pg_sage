package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// ux-02b: the optional one-sentence headline is written by the configured
// model from the aggregated counts only. Any reply that is not one plain
// sentence of at most 200 characters without an identifier falls back to
// the deterministic headline, with the reason logged. Tested against a
// fake OpenAI-compatible server.
// No concurrent access tests: one headline call per digest; the client's
// own concurrency is llm's.

type fakeModel struct {
	mu      sync.Mutex
	prompts []string
	reply   func(w http.ResponseWriter)
}

func (f *fakeModel) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		for _, m := range body.Messages {
			f.prompts = append(f.prompts, m.Content)
		}
		f.mu.Unlock()
		f.reply(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatReply(content string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage": map[string]any{"total_tokens": 9}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) fn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logSink) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func headlineWorker(t *testing.T, f *fakeModel) (*Worker, *logSink) {
	t.Helper()
	srv := f.server(t)
	logs := &logSink{}
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "test-model", TimeoutSeconds: 5, TokenBudgetDaily: 1000000}, noopLog)
	return &Worker{cfg: &config.Config{}, llm: client, logFn: logs.fn}, logs
}

func TestDigestHeadline_ModelSentenceIsUsed(t *testing.T) {
	f := &fakeModel{reply: chatReply("Six changes landed and five were verified; two " +
		"decisions wait for you.")}
	w, _ := headlineWorker(t, f)
	d := BuildDigest(happyInput())
	w.digestHeadline(context.Background(), &d)
	if d.HeadlineSource != HeadlineModel || !strings.HasPrefix(d.Headline, "Six changes") {
		t.Fatalf("headline = %q (%s), want the model's sentence", d.Headline, d.HeadlineSource)
	}
	f.mu.Lock()
	prompt := strings.Join(f.prompts, "\n")
	f.mu.Unlock()
	// Only counts reach the model: no identifier, no change text, no reason.
	for _, leak := range []string{"public.orders", "Index created", "maintenance window",
		"prod"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("the prompt carries %q:\n%s", leak, prompt)
		}
	}
	if !strings.Contains(prompt, `"changes":6`) {
		t.Errorf("the prompt lacks the counts:\n%s", prompt)
	}
}

// Invalid input, error propagation and boundaries: every bad reply gives
// the fallback headline and a logged reason.
func TestDigestHeadline_BadRepliesFallBack(t *testing.T) {
	long := strings.Repeat("a", 201)
	slow := func(w http.ResponseWriter) {
		time.Sleep(600 * time.Millisecond)
		chatReply("Too late.")(w)
	}
	limited := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}
	for name, tc := range map[string]struct {
		reply  func(http.ResponseWriter)
		reason string
	}{
		"over 200 characters": {chatReply(long), "longer than 200"},
		"names a seeded table": {chatReply("Orders on public.orders were held back."),
			"identifier"},
		"empty body":    {chatReply("   "), "empty"},
		"fenced json":   {chatReply("```json\n{\"headline\":\"6 changes\"}\n```"), "plain"},
		"malformed json": {chatReply(`{"headline": "6 changes"`), "plain"},
		"two lines":     {chatReply("Six changes.\nTwo need you."), "one line"},
		"timeout":       {slow, "model"},
		"rate limited":  {limited, "model"},
	} {
		t.Run(name, func(t *testing.T) {
			restore := headlineTimeout
			headlineTimeout = 300 * time.Millisecond
			t.Cleanup(func() { headlineTimeout = restore })
			w, logs := headlineWorker(t, &fakeModel{reply: tc.reply})
			d := BuildDigest(happyInput())
			w.digestHeadline(context.Background(), &d)
			if d.Headline != "6 changes, 5 verified, 1 reverted, 2 need you" ||
				d.HeadlineSource != HeadlineDeterministic {
				t.Fatalf("headline = %q (%s), want the fallback", d.Headline,
					d.HeadlineSource)
			}
			if !strings.Contains(d.HeadlineFallback, tc.reason) {
				t.Fatalf("fallback reason = %q, want %q", d.HeadlineFallback, tc.reason)
			}
			if !strings.Contains(logs.joined(), "digest headline") {
				t.Fatalf("log = %q, want the fallback logged", logs.joined())
			}
		})
	}
}

// Nil/empty: with no model configured, or a disabled one, the
// deterministic headline is used and nothing is logged as a failure.
func TestDigestHeadline_NoModelUsesDeterministic(t *testing.T) {
	for name, client := range map[string]*llm.Client{
		"nil":      nil,
		"disabled": llm.New(&config.LLMConfig{Enabled: false}, noopLog),
	} {
		logs := &logSink{}
		w := &Worker{cfg: &config.Config{}, llm: client, logFn: logs.fn}
		d := BuildDigest(happyInput())
		w.digestHeadline(context.Background(), &d)
		if d.HeadlineSource != HeadlineDeterministic || d.HeadlineFallback != "" ||
			d.Headline != "6 changes, 5 verified, 1 reverted, 2 need you" {
			t.Errorf("%s: headline = %q (%s, %q)", name, d.Headline, d.HeadlineSource,
				d.HeadlineFallback)
		}
		if logs.joined() != "" {
			t.Errorf("%s: logged %q", name, logs.joined())
		}
	}
}

// Boundary: exactly 200 characters is accepted.
func TestValidateHeadline_Boundary(t *testing.T) {
	d := BuildDigest(happyInput())
	ok := strings.Repeat("b", 199) + "."
	if got, err := validateHeadline(ok, d); err != nil || got != ok {
		t.Fatalf("200 characters: %q, %v; want accepted", got, err)
	}
	if _, err := validateHeadline(ok+"c", d); err == nil {
		t.Fatal("201 characters were accepted")
	}
	if _, err := validateHeadline("Work on ORDERS went well.", d); err == nil {
		t.Fatal("a table name in another case was accepted")
	}
	if got, err := validateHeadline(`  "Six changes landed."  `, d); err != nil ||
		got != "Six changes landed." {
		t.Fatalf("quoted sentence: %q, %v; want it trimmed and unquoted", got, err)
	}
}
