package briefing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func requireRenderedWarning(t *testing.T, messages []string, fragment string) string {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("warnings=%q, want exactly one", messages)
	}
	message := messages[0]
	if !strings.HasPrefix(message, "[WARN] briefing: ") ||
		!strings.Contains(message, fragment) || strings.Contains(message, "%!") ||
		strings.Contains(message, "%v") {
		t.Fatalf("warning lost severity, context, or formatted detail: %q", message)
	}
	return message
}

func TestWarningInvalidSchedulePreservesParseReason(t *testing.T) {
	logs := &logCapture{}
	cfg := config.DefaultConfig()
	cfg.Briefing.Schedule = "invalid"
	worker := New(nil, cfg, nil, logs.logFn)
	message := requireRenderedWarning(t, logs.messages, `invalid schedule "invalid"`)
	if !strings.Contains(message, "expected 5 fields, got 1") || worker.schedule.valid {
		t.Fatalf("bad schedule accepted or parse reason lost: %q", message)
	}
}

func TestWarningPersistenceFailurePreservesCause(t *testing.T) {
	pool, _ := requireDB(t)
	logs := &logCapture{}
	worker := &Worker{pool: pool, logFn: logs.logFn}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.storeBriefing(ctx, "must-not-persist-canceled-briefing", false, 0)
	message := requireRenderedWarning(t, logs.messages, "failed to store briefing:")
	if !strings.Contains(message, "context canceled") {
		t.Fatalf("persistence cause lost: %q", message)
	}
	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.briefings WHERE content_text=$1",
		"must-not-persist-canceled-briefing").Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("canceled write persisted: count=%d err=%v", count, err)
	}
}

func TestWarningLLMFallbackPreservesCauseAndStoresStructuredResult(t *testing.T) {
	pool, ctx := requireDB(t)
	server := fakeLLMServer(t, "", http.StatusBadRequest)
	defer server.Close()
	logs := &logCapture{}
	worker := New(pool, config.DefaultConfig(), newLLMClient(t, server.URL), logs.logFn)
	result, err := worker.Generate(ctx)
	if err != nil || !strings.Contains(result, "# pg_sage Health Briefing") {
		t.Fatalf("structured fallback absent: err=%v result=%q", err, result)
	}
	message := requireRenderedWarning(t, logs.messages, "LLM enhancement failed:")
	if !strings.Contains(message, "400") || !strings.Contains(message, "using structured") {
		t.Fatalf("fallback reason/result not explained: %q", message)
	}
	var used bool
	err = pool.QueryRow(ctx, `SELECT llm_used FROM sage.briefings
		WHERE content_text=$1 ORDER BY generated_at DESC LIMIT 1`, result).Scan(&used)
	if err != nil || used {
		t.Fatalf("fallback not persisted as structured: llm_used=%v err=%v", used, err)
	}
}

func TestWarningSlackMalformedURLDoesNotRevealSecret(t *testing.T) {
	logs := &logCapture{}
	cfg := config.DefaultConfig()
	cfg.Briefing.SlackWebhookURL = "http://[invalid/slack-secret-canary"
	worker := &Worker{cfg: cfg, logFn: logs.logFn}
	worker.sendSlack(context.Background(), "fixture")
	message := requireRenderedWarning(t, logs.messages, "slack request error:")
	if strings.Contains(message, "slack-secret-canary") || !strings.Contains(message, "missing") {
		t.Fatalf("malformed webhook exposed secret or lost cause: %q", message)
	}
}

func TestWarningSlackCanceledTransportDoesNotRevealSecret(t *testing.T) {
	logs := &logCapture{}
	cfg := config.DefaultConfig()
	cfg.Briefing.SlackWebhookURL = "http://127.0.0.1:1/slack-secret-canary"
	worker := &Worker{cfg: cfg, logFn: logs.logFn}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.sendSlack(ctx, "fixture")
	message := requireRenderedWarning(t, logs.messages, "slack send error:")
	leaked := strings.Contains(message, "slack-secret-canary")
	if leaked || !strings.Contains(message, "context canceled") {
		t.Fatalf("transport exposed secret or lost cause: %q", message)
	}
}

func TestWarningSlackFailureStatusIsNotSilentSuccess(t *testing.T) {
	for _, status := range []int{200, 204, 400, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			logs := &logCapture{}
			cfg := config.DefaultConfig()
			cfg.Briefing.SlackWebhookURL = server.URL + "/slack-secret-canary"
			worker := &Worker{cfg: cfg, logFn: logs.logFn}
			worker.sendSlack(context.Background(), "fixture")
			if status < 300 {
				if len(logs.messages) != 0 {
					t.Fatalf("successful dispatch warned: %q", logs.messages)
				}
				return
			}
			message := requireRenderedWarning(t, logs.messages,
				fmt.Sprintf("slack delivery rejected: HTTP %d", status))
			if strings.Contains(message, "slack-secret-canary") {
				t.Fatalf("HTTP rejection exposed webhook: %q", message)
			}
		})
	}
}

func TestWarningConcurrentCanceledDispatchesRemainFormatted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Briefing.SlackWebhookURL = "http://127.0.0.1:1/slack-secret-canary"
	messages := make(chan string, 10)
	worker := &Worker{cfg: cfg, logFn: func(level, format string, args ...any) {
		messages <- fmt.Sprintf("[%s] %s", level, fmt.Sprintf(format, args...))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var workers sync.WaitGroup
	for range 10 {
		workers.Add(1)
		go func() { defer workers.Done(); worker.sendSlack(ctx, "fixture") }()
	}
	workers.Wait()
	close(messages)
	count := 0
	for message := range messages {
		requireRenderedWarning(t, []string{message}, "slack send error:")
		count++
	}
	if count != 10 {
		t.Fatalf("lost concurrent warnings: %d", count)
	}
}
