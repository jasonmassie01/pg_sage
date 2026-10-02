package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string][]time.Time
	limit    int
	interval time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

func NewRateLimiter(maxPerMinute int) *RateLimiter {
	if maxPerMinute <= 0 {
		maxPerMinute = config.DefaultRateLimit
	}
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    maxPerMinute,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() { close(rl.stop) })
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.interval)
	ts := rl.windows[ip]
	start := 0
	for start < len(ts) && ts[start].Before(cutoff) {
		start++
	}
	ts = ts[start:]
	if len(ts) >= rl.limit {
		rl.windows[ip] = ts
		return false
	}
	rl.windows[ip] = append(ts, now)
	return true
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			rl.evictExpired(now)
		case <-rl.stop:
			return
		}
	}
}

func (rl *RateLimiter) evictExpired(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := now.Add(-rl.interval)
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
}

func rateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// defaultTrustedProxies is used when cfg.API.TrustedProxies is empty:
// X-Forwarded-For is only honoured from loopback.
var defaultTrustedProxies = []string{"127.0.0.1", "::1"}

// trustedProxyNets caches the parsed *net.IPNet list derived from
// cfg.API.TrustedProxies. Rebuilt whenever cfg is reloaded. Guarded by
// the config hot-reload lock at build time.
var (
	trustedProxyNets   []*net.IPNet
	trustedProxyNetsMu sync.RWMutex
)

// buildTrustedProxyNets parses the configured trusted-proxies list
// (plain IPs or CIDR blocks) into []*net.IPNet for O(1) matching.
// Unparseable entries are logged and skipped. Called once during
// bootstrap and from the config hot-reload path.
func buildTrustedProxyNets(entries []string) []*net.IPNet {
	if len(entries) == 0 {
		entries = defaultTrustedProxies
	}
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			// Plain IP → /32 or /128.
			ip := net.ParseIP(e)
			if ip == nil {
				logWarn("config",
					"trusted_proxies: ignoring invalid IP %q", e)
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			e = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			logWarn("config",
				"trusted_proxies: ignoring invalid CIDR %q: %v",
				e, err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

// setTrustedProxies publishes a freshly parsed list. Safe for
// concurrent reads from clientIP.
func setTrustedProxies(entries []string) {
	nets := buildTrustedProxyNets(entries)
	trustedProxyNetsMu.Lock()
	trustedProxyNets = nets
	trustedProxyNetsMu.Unlock()
}

// isTrustedProxy reports whether host (an IP literal) is in the
// configured trusted-proxies list.
func isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	trustedProxyNetsMu.RLock()
	defer trustedProxyNetsMu.RUnlock()
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return host
	}
	remote := remoteIP.String()

	// Only trust X-Forwarded-For when the immediate peer is in the
	// configured trusted-proxies list. Spoofed XFF from a direct
	// attacker is ignored, preserving per-IP rate limits.
	if !isTrustedProxy(remote) {
		return remote
	}
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if xff == "" {
		return remote
	}
	return forwardedClientIP(xff, remote)
}

// forwardedClientIP walks the proxy chain from the server back toward the
// client. The first untrusted hop is the client; entries before it are
// client-controlled and intentionally ignored. A malformed trusted-side hop
// invalidates the header and falls back to the immediate peer.
func forwardedClientIP(xff, remote string) string {
	parts := strings.Split(xff, ",")
	candidate := remote
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			return remote
		}
		candidate = ip.String()
		if !isTrustedProxy(candidate) {
			return candidate
		}
	}
	return candidate
}
