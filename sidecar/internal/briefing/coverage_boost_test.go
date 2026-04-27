package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeLLMServer returns an httptest server that responds like an
// OpenAI-compatible /chat/completions endpoint.
func fakeLLMServer(t *testing.T, responseContent string, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			fmt.Fprintf(w, `{"error":"mock error"}`)
			return
		}
		resp := llm.ChatResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Message: struct {
						Content string `json:"content"`
					}{Content: responseContent},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				TotalTokens int `json:"total_tokens"`
			}{TotalTokens: 42},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// newLLMClient creates a real llm.Client pointing at a test server.
func newLLMClient(t *testing.T, endpoint string) *llm.Client {
	t.Helper()
	cfg := &config.LLMConfig{
		Enabled:             true,
		Endpoint:            endpoint,
		APIKey:              "test-key",
		Model:               "test-model",
		TimeoutSeconds:      5,
		TokenBudgetDaily:    100000,
		ContextBudgetTokens: 4096,
		CooldownSeconds:     1,
	}
	return llm.New(cfg, func(_, _ string, _ ...any) {})
}

// collectLog captures log messages for assertion.
type logCapture struct {
	messages []string
}

func (lc *logCapture) logFn(level, msg string, args ...any) {
	lc.messages = append(lc.messages, fmt.Sprintf("[%s] %s", level, fmt.Sprintf(msg, args...)))
}

// ---------------------------------------------------------------------------
// enhanceWithLLM — 0% coverage
// ---------------------------------------------------------------------------

func TestCoverage_EnhanceWithLLM_Success(t *testing.T) {
	enhanced := "# Enhanced Briefing\n\nLLM-generated content here."
	srv := fakeLLMServer(t, enhanced, http.StatusOK)
	defer srv.Close()

	client := newLLMClient(t, srv.URL)
	cfg := &config.Config{
		LLM: config.LLMConfig{ContextBudgetTokens: 4096},
	}
	w := &Worker{
		cfg:   cfg,
		llm:   client,
		logFn: noopLog,
	}

	result, tokens, err := w.enhanceWithLLM(context.Background(), "structured data")
	if err != nil {
		t.Fatalf("enhanceWithLLM should succeed: %v", err)
	}
	if tokens != 42 {
		t.Errorf("expected 42 tokens, got %d", tokens)
	}
	if result != enhanced {
		t.Errorf("expected enhanced content %q, got %q", enhanced, result)
	}
}

func TestCoverage_EnhanceWithLLM_APIError(t *testing.T) {
	// Use 400 (Bad Request) instead of 500 to avoid retry delays.
	srv := fakeLLMServer(t, "", http.StatusBadRequest)
	defer srv.Close()

	client := newLLMClient(t, srv.URL)
	cfg := &config.Config{
		LLM: config.LLMConfig{ContextBudgetTokens: 4096},
	}
	w := &Worker{
		cfg:   cfg,
		llm:   client,
		logFn: noopLog,
	}

	_, _, err := w.enhanceWithLLM(context.Background(), "structured data")
	if err == nil {
		t.Error("enhanceWithLLM should fail on API error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention status 400, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sendSlack — 0% coverage
// ---------------------------------------------------------------------------

func TestCoverage_SendSlack_Success(t *testing.T) {
	var received atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed to decode slack payload: %v", err)
		}
		received.Store(payload["text"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			SlackWebhookURL: srv.URL,
		},
	}
	w := &Worker{
		cfg:   cfg,
		logFn: noopLog,
	}

	w.sendSlack(context.Background(), "hello from pg_sage")

	got, ok := received.Load().(string)
	if !ok || got != "hello from pg_sage" {
		t.Errorf("expected slack to receive 'hello from pg_sage', got %q", got)
	}
}

func TestCoverage_SendSlack_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	lc := &logCapture{}
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			SlackWebhookURL: srv.URL,
		},
	}
	w := &Worker{
		cfg:   cfg,
		logFn: lc.logFn,
	}

	// Should not panic; just log.
	w.sendSlack(context.Background(), "test message")
}

func TestCoverage_SendSlack_InvalidURL(t *testing.T) {
	lc := &logCapture{}
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			SlackWebhookURL: "://invalid-url",
		},
	}
	w := &Worker{
		cfg:   cfg,
		logFn: lc.logFn,
	}

	// Should not panic with malformed URL.
	w.sendSlack(context.Background(), "test message")

	found := false
	for _, msg := range lc.messages {
		if strings.Contains(msg, "slack") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a warning log about slack error")
	}
}

func TestCoverage_SendSlack_ConnectionRefused(t *testing.T) {
	// Start a server and immediately close it to get a port that refuses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	refusedURL := srv.URL
	srv.Close()

	lc := &logCapture{}
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			SlackWebhookURL: refusedURL,
		},
	}
	w := &Worker{
		cfg:   cfg,
		logFn: lc.logFn,
	}

	// Should log error, not panic.
	w.sendSlack(context.Background(), "test message")

	found := false
	for _, msg := range lc.messages {
		if strings.Contains(msg, "slack send error") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'slack send error' warning in logs")
	}
}

// ---------------------------------------------------------------------------
// Dispatch — cover the slack-with-URL path (80% -> higher)
// ---------------------------------------------------------------------------

func TestCoverage_Dispatch_SlackWithURL(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			Channels:        []string{"slack"},
			SlackWebhookURL: srv.URL,
		},
	}
	w := &Worker{
		cfg:   cfg,
		logFn: noopLog,
	}

	w.Dispatch(context.Background(), "test dispatch to slack")

	if !called.Load() {
		t.Error("expected slack webhook to be called")
	}
}

func TestCoverage_Dispatch_MultipleChannels(t *testing.T) {
	var slackCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			Channels:        []string{"stdout", "slack", "unknown_channel"},
			SlackWebhookURL: srv.URL,
		},
	}
	w := &Worker{
		cfg:   cfg,
		logFn: noopLog,
	}

	// Should not panic; unknown channels are silently skipped.
	w.Dispatch(context.Background(), "multi-channel test")

	if !slackCalled.Load() {
		t.Error("expected slack webhook to be called for multi-channel dispatch")
	}
}

// ---------------------------------------------------------------------------
// Generate — LLM enhancement path (55.6% -> higher)
// ---------------------------------------------------------------------------

func TestCoverage_Generate_WithLLM_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	enhanced := "# LLM-Enhanced Briefing\n\nEverything looks good."
	srv := fakeLLMServer(t, enhanced, http.StatusOK)
	defer srv.Close()

	client := newLLMClient(t, srv.URL)
	cfg := &config.Config{
		LLM: config.LLMConfig{ContextBudgetTokens: 4096},
	}
	w := New(pool, cfg, client, noopLog)

	result, err := w.Generate(ctx)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensureSageSchema(t, ctx)
		result, err = w.Generate(ctx)
	}
	if err != nil {
		t.Fatalf("Generate with LLM: %v", err)
	}

	if result != enhanced {
		t.Errorf("expected LLM-enhanced content, got:\n%s", result)
	}
}

func TestCoverage_Generate_LLMFallsBackOnError_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	// LLM returns 400 (Bad Request) -> no retry, fast failure,
	// should fall back to structured briefing.
	srv := fakeLLMServer(t, "", http.StatusBadRequest)
	defer srv.Close()

	client := newLLMClient(t, srv.URL)
	lc := &logCapture{}
	cfg := &config.Config{
		LLM: config.LLMConfig{ContextBudgetTokens: 4096},
	}
	w := New(pool, cfg, client, lc.logFn)

	result, err := w.Generate(ctx)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensureSageSchema(t, ctx)
		result, err = w.Generate(ctx)
	}
	if err != nil {
		t.Fatalf("Generate should succeed with fallback: %v", err)
	}

	// Should contain structured briefing header (not LLM-enhanced).
	if !strings.Contains(result, "# pg_sage Health Briefing") {
		t.Error("expected structured briefing header in fallback output")
	}

	// Should have logged a warning about LLM failure.
	foundWarning := false
	for _, msg := range lc.messages {
		if strings.Contains(msg, "LLM enhancement failed") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Error("expected 'LLM enhancement failed' warning in logs")
	}
}

// ---------------------------------------------------------------------------
// storeBriefing — cover the error path (75% -> higher)
// ---------------------------------------------------------------------------

func TestCoverage_StoreBriefing_InvalidTable_LivePG(t *testing.T) {
	// Use a real pool but exercise the store path with both llm=true and
	// llm=false to cover both branches of the function.
	pool, ctx := requireDB(t)

	lc := &logCapture{}
	w := &Worker{
		pool:  pool,
		cfg:   &config.Config{},
		logFn: lc.logFn,
	}

	// Store without LLM — should succeed.
	w.storeBriefing(ctx, "no-llm briefing test", false, 0)

	// Verify the row was inserted.
	var content string
	err := pool.QueryRow(ctx,
		`SELECT content_text FROM sage.briefings
		 WHERE content_text = 'no-llm briefing test'
		 ORDER BY generated_at DESC LIMIT 1`).Scan(&content)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		t.Skipf("sage schema dropped by concurrent tests: %v", err)
	}
	if err != nil {
		t.Fatalf("expected stored briefing row: %v", err)
	}
	if content != "no-llm briefing test" {
		t.Errorf("content mismatch: got %q", content)
	}

	// Cleanup.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM sage.briefings WHERE content_text = 'no-llm briefing test'`)
	})
}

func TestCoverage_StoreBriefing_WithLLMTokens_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	w := &Worker{
		pool:  pool,
		cfg:   &config.Config{},
		logFn: noopLog,
	}

	// Store with LLM used = true and token count.
	w.storeBriefing(ctx, "llm-enhanced briefing content", true, 150)

	// Verify it was stored.
	var content string
	var llmUsed bool
	var tokenCount int
	err := pool.QueryRow(ctx, `
		SELECT content_text, llm_used, token_count
		FROM sage.briefings
		WHERE content_text = 'llm-enhanced briefing content'
		ORDER BY generated_at DESC LIMIT 1
	`).Scan(&content, &llmUsed, &tokenCount)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		t.Skipf("sage schema dropped by concurrent tests: %v", err)
	}
	if err != nil {
		t.Fatalf("querying stored briefing: %v", err)
	}
	if content != "llm-enhanced briefing content" {
		t.Errorf("content mismatch: got %q", content)
	}
	if !llmUsed {
		t.Error("expected llm_used=true")
	}
	if tokenCount != 150 {
		t.Errorf("expected token_count=150, got %d", tokenCount)
	}

	// Cleanup.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM sage.briefings WHERE content_text = 'llm-enhanced briefing content'`)
	})
}

// ---------------------------------------------------------------------------
// parseCron — cover remaining error branches (82.4% -> higher)
// ---------------------------------------------------------------------------

func TestCoverage_ParseCron_FieldErrors(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string // substring expected in error
	}{
		{
			name: "invalid dom field",
			expr: "0 6 abc * *",
			want: "dom",
		},
		{
			name: "invalid month field",
			expr: "0 6 * abc *",
			want: "month",
		},
		{
			name: "invalid dow field",
			expr: "0 6 * * abc",
			want: "dow",
		},
		{
			name: "dom out of range high",
			expr: "0 6 32 * *",
			want: "dom",
		},
		{
			name: "month out of range high",
			expr: "0 6 * 13 *",
			want: "month",
		},
		{
			name: "dow out of range high",
			expr: "0 6 * * 7",
			want: "dow",
		},
		{
			name: "too few fields",
			expr: "0 6 *",
			want: "expected 5 fields",
		},
		{
			name: "too many fields",
			expr: "0 6 * * * *",
			want: "expected 5 fields",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCron(tt.expr)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.expr)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should contain %q", err.Error(), tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCronPart — cover remaining edge cases (88.9% -> higher)
// ---------------------------------------------------------------------------

func TestCoverage_ParseCronPart_InvalidStep(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("*/0", 0, 59, out)
	if err == nil {
		t.Error("step of 0 should be rejected")
	}
	if err != nil && !strings.Contains(err.Error(), "invalid step") {
		t.Errorf("error should mention 'invalid step', got: %v", err)
	}
}

func TestCoverage_ParseCronPart_NegativeStep(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("*/-1", 0, 59, out)
	if err == nil {
		t.Error("negative step should be rejected")
	}
}

func TestCoverage_ParseCronPart_InvalidRangeStart(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("abc-10", 0, 59, out)
	if err == nil {
		t.Error("non-numeric range start should be rejected")
	}
	if err != nil && !strings.Contains(err.Error(), "invalid range start") {
		t.Errorf("error should mention 'invalid range start', got: %v", err)
	}
}

func TestCoverage_ParseCronPart_InvalidRangeEnd(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("10-abc", 0, 59, out)
	if err == nil {
		t.Error("non-numeric range end should be rejected")
	}
	if err != nil && !strings.Contains(err.Error(), "invalid range end") {
		t.Errorf("error should mention 'invalid range end', got: %v", err)
	}
}

func TestCoverage_ParseCronPart_RangeInverted(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("30-10", 0, 59, out)
	if err == nil {
		t.Error("inverted range (30-10) should be rejected")
	}
	if err != nil && !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error should mention 'out of range', got: %v", err)
	}
}

func TestCoverage_ParseCronPart_RangeBelowMin(t *testing.T) {
	out := make([]bool, 31) // doms: min=1, max=31
	err := parseCronPart("0", 1, 31, out)
	if err == nil {
		t.Error("value below min should be rejected")
	}
}

func TestCoverage_ParseCronPart_StepWithRange(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("10-30/5", 0, 59, out)
	if err != nil {
		t.Fatalf("valid range with step should succeed: %v", err)
	}
	// Should set 10, 15, 20, 25, 30.
	expected := map[int]bool{10: true, 15: true, 20: true, 25: true, 30: true}
	for i := 0; i < 60; i++ {
		if out[i] != expected[i] {
			t.Errorf("out[%d] = %v, want %v", i, out[i], expected[i])
		}
	}
}

func TestCoverage_ParseCronPart_NonNumericStep(t *testing.T) {
	out := make([]bool, 60)
	err := parseCronPart("*/abc", 0, 59, out)
	if err == nil {
		t.Error("non-numeric step should be rejected")
	}
	if err != nil && !strings.Contains(err.Error(), "invalid step") {
		t.Errorf("error should mention 'invalid step', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// New — cover the invalid-schedule warning path
// ---------------------------------------------------------------------------

func TestCoverage_New_InvalidScheduleLogsWarning(t *testing.T) {
	lc := &logCapture{}
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			Schedule: "not-a-cron",
		},
	}

	w := New(nil, cfg, nil, lc.logFn)
	if w == nil {
		t.Fatal("New should return non-nil even with invalid schedule")
	}
	if w.schedule.valid {
		t.Error("schedule should be invalid for 'not-a-cron'")
	}

	foundWarning := false
	for _, msg := range lc.messages {
		if strings.Contains(msg, "invalid schedule") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Error("expected 'invalid schedule' warning in logs")
	}
}

// ---------------------------------------------------------------------------
// cronSchedule.matches — boundary cases
// ---------------------------------------------------------------------------

func TestCoverage_CronMatches_AllFields(t *testing.T) {
	// "30 14 15 6 3" = minute 30, hour 14, day 15, June, Wednesday
	s, err := parseCron("30 14 15 6 3")
	if err != nil {
		t.Fatal(err)
	}

	// 2026-06-17 is a Wednesday, but day is 17, not 15 -> should not match.
	wed17 := time.Date(2026, 6, 17, 14, 30, 0, 0, time.UTC)
	if s.matches(wed17) {
		t.Error("should not match: day 17 != 15")
	}

	// Need a date that is June 15 AND a Wednesday.
	// June 15, 2022 is a Wednesday.
	wed15 := time.Date(2022, 6, 15, 14, 30, 0, 0, time.UTC)
	if wed15.Weekday() != time.Wednesday {
		t.Skipf("June 15, 2022 is not Wednesday: %s", wed15.Weekday())
	}
	if !s.matches(wed15) {
		t.Error("should match June 15 2022 14:30 Wed")
	}
}

func TestCoverage_CronMatches_MonthBoundary(t *testing.T) {
	// "0 0 1 1 *" = midnight Jan 1st
	s, err := parseCron("0 0 1 1 *")
	if err != nil {
		t.Fatal(err)
	}

	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.matches(jan1) {
		t.Error("should match midnight Jan 1st")
	}

	feb1 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if s.matches(feb1) {
		t.Error("should not match Feb 1st for '0 0 1 1 *'")
	}
}

// ---------------------------------------------------------------------------
// ShouldRun — edge case: zero lastRun
// ---------------------------------------------------------------------------

func TestCoverage_ShouldRun_ZeroLastRun(t *testing.T) {
	s, _ := parseCron("* * * * *") // every minute
	w := &Worker{schedule: s}
	// lastRun is zero — should allow running.
	now := time.Date(2026, 3, 27, 14, 0, 0, 0, time.UTC)
	if !w.ShouldRun(now) {
		t.Error("should run when lastRun is zero")
	}
}

func TestCoverage_ShouldRun_ExactlyAt30s(t *testing.T) {
	s, _ := parseCron("* * * * *")
	w := &Worker{
		schedule: s,
		lastRun:  time.Date(2026, 3, 27, 14, 0, 0, 0, time.UTC),
	}
	// Exactly 30 seconds later — debounce is `< 30s`, so 30s is NOT < 30s,
	// meaning the debounce no longer blocks. Should be allowed to run.
	now := time.Date(2026, 3, 27, 14, 0, 30, 0, time.UTC)
	if !w.ShouldRun(now) {
		t.Error("should run at exactly 30s (debounce is < 30s, 30s is not blocked)")
	}
}
