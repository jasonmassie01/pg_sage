package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"2", 2 * time.Second, true},
		{"0", 0, true},
		{" 7 ", 7 * time.Second, true},
		{"45", maxRetryAfter, true}, // capped at 30 s
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true}, // past date
		{now.Add(time.Hour).Format(http.TimeFormat), maxRetryAfter, true},
		{"", 0, false},
		{"-3", 0, false},
		{"soon", 0, false},
		{"1.5", 0, false},
		{"99999999999999999999", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.header, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %s,%v want %s,%v", tc.header,
				got, ok, tc.want, tc.ok)
		}
	}
}

// Retry-After: 2 delays the retry by at least 2 s, even though the
// client's own ladder would retry at once.
func TestRetryAfter_SecondsHonored(t *testing.T) {
	s := newScriptServer(t, step{status: 429, retryAfter: "2"}, step{})
	c := scriptClient(s, "m", 10)
	content, _, err := c.Chat(context.Background(), "s", "u", 10)
	if err != nil || content != "ok" {
		t.Fatalf("Chat = %q, %v; want ok after the retry", content, err)
	}
	if n := s.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
	if gap := s.gap(1); gap < 2*time.Second {
		t.Fatalf("retry after %s, want at least 2s", gap)
	}
	if failureCount(c) != 0 {
		t.Fatalf("a 429 that recovered counted %d failures", failureCount(c))
	}
}

// A Retry-After the caller's deadline cannot cover returns at once with
// the rate-limit error instead of sleeping into the deadline.
func TestRetryAfter_PastDeadlineReturnsAtOnce(t *testing.T) {
	s := newScriptServer(t, step{status: 429, retryAfter: "20"})
	c := scriptClient(s, "m", 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := c.Chat(ctx, "s", "u", 10)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %s, want at once", elapsed)
	}
	if !errors.Is(err, ErrRateLimited) || ErrorClass(err) != "rate" {
		t.Fatalf("err = %v class=%q, want the rate limit", err, ErrorClass(err))
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	if failureCount(c) != 1 {
		t.Fatalf("failures = %d, want the final 429 counted once", failureCount(c))
	}
}

// A 502 then a 200 succeeds in two attempts, charged once.
func TestRetry_BadGatewayThenOK(t *testing.T) {
	s := newScriptServer(t, step{status: 502}, step{})
	c := scriptClient(s, "m", 10)
	content, tokens, err := c.Chat(context.Background(), "s", "u", 10)
	if err != nil || content != "ok" || tokens != 10 {
		t.Fatalf("Chat = %q,%d,%v; want ok,10,nil", content, tokens, err)
	}
	if n := s.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
	if used := c.TokensUsedToday(); used != 10 {
		t.Fatalf("tokens charged = %d, want 10 (one charge per request)", used)
	}
	if failureCount(c) != 0 || circuitState(c) != "closed" {
		t.Fatalf("failures=%d state=%s", failureCount(c), circuitState(c))
	}
}

// Boundary: a provider that never recovers gets exactly maxAttempts
// requests, then one counted failure.
func TestRetry_AttemptsCapped(t *testing.T) {
	s := newScriptServer(t, step{status: 503})
	c := scriptClient(s, "m", 10)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if ErrorClass(err) != "server" {
		t.Fatalf("err = %v, want server class", err)
	}
	if n := s.calls.Load(); n != 4 {
		t.Fatalf("calls = %d, want 4", n)
	}
	if failureCount(c) != 1 {
		t.Fatalf("failures = %d, want 1", failureCount(c))
	}
}

// The default ladder waits between attempts (1 s before the second).
func TestRetry_DefaultLadderWaits(t *testing.T) {
	s := newScriptServer(t, step{status: 500}, step{})
	c := scriptClient(s, "m", 10)
	c.retryDelays = defaultRetryDelays()
	if _, _, err := c.Chat(context.Background(), "s", "u", 10); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gap := s.gap(1); gap < 900*time.Millisecond {
		t.Fatalf("second attempt after %s, want about 1s", gap)
	}
}

// Each attempt gets its own timeout_seconds: two slow attempts that each
// fit the per-attempt deadline succeed although together they exceed it.
func TestRetry_PerAttemptDeadline(t *testing.T) {
	s := newScriptServer(t,
		step{status: 503, delay: 700 * time.Millisecond},
		step{delay: 700 * time.Millisecond})
	c := scriptClient(s, "m", 1)
	content, _, err := c.Chat(context.Background(), "s", "u", 10)
	if err != nil || content != "ok" {
		t.Fatalf("Chat = %q, %v; want ok (1.4s over two 1s attempts)", content, err)
	}
}

// A timed-out attempt is not retried and counts once.
func TestRetry_TimeoutNotRetried(t *testing.T) {
	s := newScriptServer(t, step{delay: 3 * time.Second})
	c := scriptClient(s, "plain-model", 1)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if !errors.Is(err, context.DeadlineExceeded) || ErrorClass(err) != "timeout" {
		t.Fatalf("err = %v class=%q, want a timeout", err, ErrorClass(err))
	}
	if s.calls.Load() != 1 || failureCount(c) != 1 {
		t.Fatalf("calls=%d failures=%d, want 1/1", s.calls.Load(), failureCount(c))
	}
}

func TestAttemptTimeout(t *testing.T) {
	cases := []struct {
		model   string
		seconds int
		want    time.Duration
	}{
		{"plain-model", 30, 30 * time.Second},
		{"plain-model", 0, config.DefaultLLMTimeoutSeconds * time.Second},
		{"plain-model", -5, config.DefaultLLMTimeoutSeconds * time.Second},
		{"o3-mini", 30, config.DefaultOptLLMTimeoutSeconds * time.Second},
		{"gemini-2.5-pro", 0, config.DefaultOptLLMTimeoutSeconds * time.Second},
		{"o3-mini", 300, 300 * time.Second},
	}
	for _, tc := range cases {
		cfg := config.LLMConfig{Model: tc.model, TimeoutSeconds: tc.seconds}
		if got := attemptTimeout(cfg); got != tc.want {
			t.Errorf("attemptTimeout(%s, %d) = %s, want %s", tc.model,
				tc.seconds, got, tc.want)
		}
	}
}

// A reasoning model that answers in 35 s succeeds with timeout_seconds:
// 30, because its per-attempt floor is 120 s.
func TestThinkingModelSlowAnswerSucceeds(t *testing.T) {
	s := newScriptServer(t, step{delay: 35 * time.Second})
	c := scriptClient(s, "o3-mini", 30)
	content, _, err := c.Chat(context.Background(), "s", "u", 10)
	if err != nil || content != "ok" {
		t.Fatalf("Chat = %q, %v; want ok after 35s", content, err)
	}
	if failureCount(c) != 0 {
		t.Fatalf("slow reasoning answer counted %d failures", failureCount(c))
	}
}
