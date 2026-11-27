package briefing

import (
	"context"
	"net/http"
	"strings"
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
	worker.storeBriefing(ctx, "must-not-persist-canceled-briefing", false, 0, Payload{})
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
