package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// footprint-07: time spent waiting on the provider is not local work. A
// chat inside a tracked loop run adds its elapsed time to the run's
// wait, which the loop tracker records as llm_wait:<database>.

func slowProvider(t *testing.T, delay time.Duration, status int) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":5}}`)
		}))
	t.Cleanup(server.Close)
	return server.URL
}

func waitClient(url string) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "test-key",
		Model: "test-model", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
		func(string, string, ...any) {})
}

func TestChat_WaitIsRecordedOnTheTrackedRun(t *testing.T) {
	c := waitClient(slowProvider(t, 80*time.Millisecond, http.StatusOK))
	loops := selfbudget.NewLoops()
	ctx, done := loops.TrackFor(selfbudget.WithDatabase(context.Background(), "db1"),
		"briefing")
	if _, _, err := c.Chat(ctx, "sys", "hello", 50); err != nil {
		t.Fatalf("chat: %v", err)
	}
	done()
	snap := loops.Snapshot()
	wait := snap["llm_wait:db1"]
	if wait.Runs != 1 || wait.Busy < 80*time.Millisecond || wait.Busy > 5*time.Second {
		t.Fatalf("llm_wait:db1 = %+v, want one run of at least 80ms", wait)
	}
	if busy := snap["briefing:db1"]; busy.Runs != 1 || busy.Busy >= wait.Busy {
		t.Fatalf("briefing:db1 = %+v, want its local time below the %v wait", busy,
			wait.Busy)
	}
}

// A failed exchange waited on the provider too.
func TestChat_FailedExchangeStillCountsAsWait(t *testing.T) {
	c := waitClient(slowProvider(t, 40*time.Millisecond, http.StatusBadRequest))
	loops := selfbudget.NewLoops()
	ctx, done := loops.TrackFor(selfbudget.WithDatabase(context.Background(), "db2"),
		"sre")
	if _, _, err := c.Chat(ctx, "sys", "hello", 50); err == nil {
		t.Fatal("a 400 must fail the chat")
	}
	done()
	if wait := loops.Snapshot()["llm_wait:db2"]; wait.Busy < 40*time.Millisecond {
		t.Fatalf("llm_wait:db2 = %+v, want the 40ms the provider took", wait)
	}
}

// Nil/empty: outside a tracked run there is no Wait in ctx and the chat
// behaves as before.
func TestChat_WithoutATrackedRunRecordsNothing(t *testing.T) {
	c := waitClient(slowProvider(t, 0, http.StatusOK))
	before := selfbudget.Process().Snapshot()
	if content, _, err := c.Chat(context.Background(), "sys", "hello", 50); err != nil ||
		content != "ok" {
		t.Fatalf("chat = %q, %v", content, err)
	}
	after := selfbudget.Process().Snapshot()
	if after[selfbudget.LLMWaitLoop] != before[selfbudget.LLMWaitLoop] {
		t.Fatalf("an untracked chat recorded a wait: %+v", after[selfbudget.LLMWaitLoop])
	}
}

// The tool-calling path waits on the provider as well.
func TestChatWithTools_WaitIsRecorded(t *testing.T) {
	c := waitClient(slowProvider(t, 60*time.Millisecond, http.StatusOK))
	loops := selfbudget.NewLoops()
	ctx, done := loops.TrackFor(selfbudget.WithDatabase(context.Background(), "db3"),
		"sre")
	_, _ = c.ChatWithTools(ctx, []llm.Message{{Role: "user", Content: "hi"}}, nil,
		llm.ToolOptions{MaxTokens: 50})
	done()
	if wait := loops.Snapshot()["llm_wait:db3"]; wait.Busy < 60*time.Millisecond {
		t.Fatalf("llm_wait:db3 = %+v, want the 60ms the provider took", wait)
	}
}
