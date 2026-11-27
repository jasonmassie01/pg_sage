package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassify_Table(t *testing.T) {
	timeoutErr := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	cases := []struct {
		name   string
		status int
		err    error
		want   errClass
	}{
		{"400 context length", 400, nil, classCaller},
		{"404 unknown model", 404, nil, classCaller},
		{"413 payload too large", 413, nil, classCaller},
		{"422 unprocessable", 422, nil, classCaller},
		{"401 bad key", 401, nil, classAuth},
		{"403 forbidden", 403, nil, classAuth},
		{"429 rate limited", 429, nil, classRate},
		{"500", 500, nil, classServer},
		{"502", 502, nil, classServer},
		{"503", 503, nil, classServer},
		{"504", 504, nil, classServer},
		{"501 other 5xx", 501, nil, classServer},
		{"302 redirect not followed", 302, nil, classServer},
		{"408 request timeout", 408, nil, classTimeout},
		{"deadline", 0, context.DeadlineExceeded, classTimeout},
		{"wrapped deadline", 0, fmt.Errorf("x: %w", context.DeadlineExceeded),
			classTimeout},
		{"net timeout", 0, timeoutErr, classTimeout},
		{"refused", 0, errors.New("connect: connection refused"), classTransport},
		{"eof", 0, io.ErrUnexpectedEOF, classTransport},
		{"200 ok", 200, nil, classNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.status, nil, tc.err); got != tc.want {
				t.Fatalf("classify(%d, %v) = %q, want %q", tc.status, tc.err,
					got, tc.want)
			}
		})
	}
}

// Nil/empty/zero: no status and no error is not a failure.
func TestClassify_ZeroIsNone(t *testing.T) {
	if got := classify(0, nil, nil); got != classNone {
		t.Fatalf("classify(0, nil, nil) = %q, want none", got)
	}
	if got := ErrorClass(nil); got != "" {
		t.Fatalf("ErrorClass(nil) = %q, want empty", got)
	}
	if got := ErrorClass(errors.New("plain")); got != "" {
		t.Fatalf("ErrorClass(plain) = %q, want empty", got)
	}
}

// Three context-length 400s are the caller's fault: the breaker stays
// closed and every error matches ErrRequestRejected.
func TestErrClass_ThreeBadRequestsLeaveBreakerClosed(t *testing.T) {
	s := newScriptServer(t, step{status: 400,
		body: `{"error":{"message":"maximum context length exceeded"}}`})
	c := scriptClient(s, "m", 5)
	for i := 0; i < 3; i++ {
		_, _, err := c.Chat(context.Background(), "s", fmt.Sprintf("u%d", i), 10)
		if !errors.Is(err, ErrRequestRejected) {
			t.Fatalf("call %d err = %v, want ErrRequestRejected", i, err)
		}
		if !strings.Contains(err.Error(), "400") {
			t.Fatalf("call %d err = %v, want the provider status", i, err)
		}
		if ErrorClass(err) != "caller" {
			t.Fatalf("call %d class = %q, want caller", i, ErrorClass(err))
		}
	}
	if c.IsCircuitOpen() || failureCount(c) != 0 || circuitState(c) != "closed" {
		t.Fatalf("open=%v failures=%d state=%s, want closed with no failures",
			c.IsCircuitOpen(), failureCount(c), circuitState(c))
	}
	if n := s.calls.Load(); n != 3 {
		t.Fatalf("provider calls = %d, want 3 (a 400 is never retried)", n)
	}
	if got := StatusOf(c).LastErrorClass; got != "caller" {
		t.Fatalf("last_error_class = %q, want caller", got)
	}
}

// A single 401 opens the breaker at once, with the reason.
func TestErrClass_AuthOpensBreakerImmediately(t *testing.T) {
	s := newScriptServer(t, step{status: 401})
	c := scriptClient(s, "m", 5)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if ErrorClass(err) != "auth" || errors.Is(err, ErrRequestRejected) {
		t.Fatalf("err = %v class=%q, want auth", err, ErrorClass(err))
	}
	if !c.IsCircuitOpen() || circuitState(c) != "open" {
		t.Fatalf("open=%v state=%s, want open after one 401",
			c.IsCircuitOpen(), circuitState(c))
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("provider calls = %d, want 1 (auth is never retried)", n)
	}
	_, _, err = c.Chat(context.Background(), "s", "u2", 10)
	if !errors.Is(err, ErrCircuitOpen) || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("next call err = %v, want ErrCircuitOpen naming auth", err)
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("open breaker still sent a request (calls=%d)", n)
	}
}

// A 403 is auth too.
func TestErrClass_ForbiddenIsAuth(t *testing.T) {
	s := newScriptServer(t, step{status: 403})
	c := scriptClient(s, "m", 5)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if ErrorClass(err) != "auth" || !c.IsCircuitOpen() {
		t.Fatalf("class=%q open=%v, want auth and open", ErrorClass(err),
			c.IsCircuitOpen())
	}
}

// A tool-path 429 returns ErrRateLimited without counting: a breaker one
// failure from opening stays closed, and the next 200 resets it.
func TestErrClass_ToolRateLimitDoesNotCount(t *testing.T) {
	ts := newToolServer(t, statusReply(http.StatusTooManyRequests),
		toolReply("done", nil, 10))
	c := toolClient(ts.srv.URL)
	c.mu.Lock()
	c.failures = 2
	c.mu.Unlock()
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		ToolOptions{})
	if !errors.Is(err, ErrRateLimited) || ErrorClass(err) != "rate" {
		t.Fatalf("err = %v class=%q, want ErrRateLimited/rate", err, ErrorClass(err))
	}
	if failureCount(c) != 2 || c.IsCircuitOpen() {
		t.Fatalf("failures=%d open=%v, want the 429 uncounted",
			failureCount(c), c.IsCircuitOpen())
	}
	if _, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if failureCount(c) != 0 || circuitState(c) != "closed" {
		t.Fatalf("failures=%d state=%s, want reset by the 200",
			failureCount(c), circuitState(c))
	}
}

// Server errors are retried, then counted once per request.
func TestErrClass_ServerErrorCountedOncePerRequest(t *testing.T) {
	s := newScriptServer(t, step{status: 500})
	c := scriptClient(s, "m", 5)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if ErrorClass(err) != "server" || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v class=%q, want server naming 500", err, ErrorClass(err))
	}
	if n := s.calls.Load(); n != maxAttempts {
		t.Fatalf("provider calls = %d, want %d", n, maxAttempts)
	}
	if failureCount(c) != 1 || c.IsCircuitOpen() {
		t.Fatalf("failures=%d open=%v, want exactly one counted failure",
			failureCount(c), c.IsCircuitOpen())
	}
}

// A malformed 200 is a provider failure (counted once, not retried).
func TestErrClass_MalformedBodyCounted(t *testing.T) {
	s := newScriptServer(t, step{body: "{not json"})
	c := scriptClient(s, "m", 5)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if ErrorClass(err) != "malformed" {
		t.Fatalf("err = %v class=%q, want malformed", err, ErrorClass(err))
	}
	if failureCount(c) != 1 || s.calls.Load() != 1 {
		t.Fatalf("failures=%d calls=%d, want 1/1", failureCount(c), s.calls.Load())
	}
}

// Caller cancellation is never a provider failure, and neither is an
// error from a request the client was reconfigured away from.
func TestErrClass_CallerCancelNotCounted(t *testing.T) {
	s := newScriptServer(t, step{status: 503})
	c := scriptClient(s, "m", 5)
	c.retryDelays = []time.Duration{time.Minute, time.Minute, time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for s.calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, _, err := c.Chat(ctx, "s", "u", 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if failureCount(c) != 0 || ErrorClass(err) != "canceled" {
		t.Fatalf("failures=%d class=%q, want an uncounted cancellation",
			failureCount(c), ErrorClass(err))
	}
}
