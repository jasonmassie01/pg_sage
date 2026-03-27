package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Sliding-window rate limiter (per IP)
// ---------------------------------------------------------------------------

type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string][]time.Time
	limit    int
	interval time.Duration
	stop     chan struct{}
}

func NewRateLimiter(maxPerMinute int) *RateLimiter {
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    maxPerMinute,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

// Stop terminates the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stop)
}

// Allow checks whether the IP is within rate limits and records the request.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.interval)

	// Prune old entries
	timestamps := rl.windows[ip]
	start := 0
	for start < len(timestamps) && timestamps[start].Before(cutoff) {
		start++
	}
	timestamps = timestamps[start:]

	if len(timestamps) >= rl.limit {
		rl.windows[ip] = timestamps
		return false
	}

	rl.windows[ip] = append(timestamps, now)
	return true
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.interval)
			for ip, ts := range rl.windows {
				start := 0
				for start < len(ts) && ts[start].Before(cutoff) {
					start++
				}
				if start >= len(ts) {
					delete(rl.windows, ip)
				} else {
					rl.windows[ip] = ts[start:]
				}
			}
			rl.mu.Unlock()
		case <-rl.stop:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP middleware
// ---------------------------------------------------------------------------

func rateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.Allow(ip) {
			logRateLimited(r.Context(), ip, r.Method, r.URL.Path)
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	// Check X-Forwarded-For first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func logRateLimited(ctx context.Context, ip, method, path string) {
	logWarn("ratelimit", "blocked %s %s %s", ip, method, path)

	// Best-effort insert into audit log
	table := "sage.mcp_log"
	if !extensionAvailable {
		table = "public.sage_mcp_log"
	}
	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx2,
		fmt.Sprintf(`INSERT INTO %s (client_ip, method, resource_uri, tool_name, tokens_used, duration_ms, status, error_message)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, table),
		ip, method, path, nil, 0, 0, "rate_limited", "rate limit exceeded",
	); err != nil {
		logWarn("audit", "rate-limit log failed: %v", err)
	}
}
