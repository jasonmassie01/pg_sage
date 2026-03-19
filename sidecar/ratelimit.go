package main

import (
	"context"
	"log"
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
}

func NewRateLimiter(maxPerMinute int) *RateLimiter {
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    maxPerMinute,
		interval: time.Minute,
	}
	// Background cleanup every 2 minutes
	go rl.cleanup()
	return rl
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
	for range ticker.C {
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
	log.Printf("[ratelimit] blocked %s %s %s", ip, method, path)

	// Best-effort insert into sage.mcp_log
	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx2,
		`INSERT INTO sage.mcp_log (client_ip, method, resource_uri, tool_name, tokens_used, duration_ms, status, error_message)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		ip, method, path, nil, 0, 0, "rate_limited", "rate limit exceeded",
	)
}
