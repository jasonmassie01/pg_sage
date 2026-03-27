package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter_AllowsUnderLimit(t *testing.T) {
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    5,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	for i := 0; i < 5; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
}

func TestRateLimiter_BlocksOverLimit(t *testing.T) {
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    3,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	for i := 0; i < 3; i++ {
		rl.Allow("1.2.3.4")
	}
	if rl.Allow("1.2.3.4") {
		t.Error("4th request should be blocked")
	}
}

func TestRateLimiter_DifferentIPs(t *testing.T) {
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    1,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	if !rl.Allow("1.1.1.1") {
		t.Error("first IP should be allowed")
	}
	if !rl.Allow("2.2.2.2") {
		t.Error("second IP should be allowed")
	}
	if rl.Allow("1.1.1.1") {
		t.Error("first IP should be blocked on second request")
	}
}

func TestRateLimiter_Stop(t *testing.T) {
	rl := NewRateLimiter(10)
	// Should not panic.
	rl.Stop()
}

func TestRateLimitMiddleware_Returns429(t *testing.T) {
	// rateLimitMiddleware's 429 path calls logRateLimited which writes to the
	// database, so this test requires a live PG connection.
	requirePG(t)

	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    1,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	handler := rateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request passes.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("first request: expected 200, got %d", rec.Code)
	}

	// Second request blocked.
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second request: expected 429, got %d", rec2.Code)
	}
}

func TestClientIP_XForwardedFor(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	if got := clientIP(req); got != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %s", got)
	}
}

func TestClientIP_RemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.1:5678"
	if got := clientIP(req); got != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %s", got)
	}
}
