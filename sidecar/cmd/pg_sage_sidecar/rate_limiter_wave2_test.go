package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestWave2RateLimiterBoundaryAllows60Rejects61(t *testing.T) {
	rl := NewRateLimiter(60)
	t.Cleanup(rl.Stop)
	var handled atomic.Int64
	handler := rateLimitMiddleware(rl, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			handled.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))

	for requestNo := 1; requestNo <= 61; requestNo++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
		req.RemoteAddr = "198.51.100.10:4321"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if requestNo <= 60 && rec.Code != http.StatusNoContent {
			t.Fatalf("request %d status = %d, want %d",
				requestNo, rec.Code, http.StatusNoContent)
		}
		if requestNo == 61 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request 61 status = %d, want 429", rec.Code)
		}
	}
	if got := handled.Load(); got != 60 {
		t.Fatalf("downstream calls = %d, want exactly 60", got)
	}
}

func TestWave2RateLimiterIsolatesClientIPs(t *testing.T) {
	rl := NewRateLimiter(60)
	t.Cleanup(rl.Stop)

	for requestNo := 1; requestNo <= 60; requestNo++ {
		if !rl.Allow("198.51.100.11") {
			t.Fatalf("client A request %d unexpectedly rejected", requestNo)
		}
	}
	if rl.Allow("198.51.100.11") {
		t.Fatal("client A request 61 was allowed")
	}
	if !rl.Allow("198.51.100.12") {
		t.Fatal("client B first request inherited client A's exhausted window")
	}
}

func TestWave2ClientIPHonorsOnlyTrustedProxyHeaders(t *testing.T) {
	restoreTrustedProxies := snapshotTrustedProxyNets()
	t.Cleanup(restoreTrustedProxies)
	setTrustedProxies([]string{"10.0.0.0/8"})

	trusted := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	trusted.RemoteAddr = "10.2.3.4:443"
	trusted.Header.Set("X-Forwarded-For", "203.0.113.8, 10.9.8.7")
	if got := clientIP(trusted); got != "203.0.113.8" {
		t.Fatalf("trusted proxy client IP = %q, want 203.0.113.8", got)
	}

	untrusted := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	untrusted.RemoteAddr = "198.51.100.77:443"
	untrusted.Header.Set("X-Forwarded-For", "203.0.113.99")
	if got := clientIP(untrusted); got != "198.51.100.77" {
		t.Fatalf("untrusted peer spoofed client IP as %q", got)
	}
}

func TestWave2ClientIPIgnoresSpoofedPrefix(t *testing.T) {
	restoreTrustedProxies := snapshotTrustedProxyNets()
	t.Cleanup(restoreTrustedProxies)
	setTrustedProxies([]string{"10.0.0.0/8"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	req.RemoteAddr = "10.2.3.4:443"
	req.Header.Set("X-Forwarded-For",
		"192.0.2.66, 203.0.113.8, 10.9.8.7")
	if got := clientIP(req); got != "203.0.113.8" {
		t.Fatalf("client IP = %q, want first untrusted hop 203.0.113.8", got)
	}

	req.Header.Set("X-Forwarded-For", "not-an-ip, 203.0.113.8")
	if got := clientIP(req); got != "203.0.113.8" {
		t.Fatalf("spoofed malformed prefix changed client IP to %q", got)
	}
}

func TestWave2ClientIPRejectsMalformedTrustedSideHop(t *testing.T) {
	restoreTrustedProxies := snapshotTrustedProxyNets()
	t.Cleanup(restoreTrustedProxies)
	setTrustedProxies([]string{"10.0.0.0/8"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	req.RemoteAddr = "10.2.3.4:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.8, not-an-ip")
	if got := clientIP(req); got != "10.2.3.4" {
		t.Fatalf("malformed trusted-side hop resolved to %q, want peer", got)
	}
}

func TestWave2RateLimiterExpiredWindowRestoresCapacity(t *testing.T) {
	const ip = "198.51.100.20"
	rl := &RateLimiter{
		windows:  map[string][]time.Time{},
		limit:    1,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	rl.windows[ip] = []time.Time{time.Now().Add(-2 * time.Minute)}

	if !rl.Allow(ip) {
		t.Fatal("expired request still consumed the client's capacity")
	}
	if got := len(rl.windows[ip]); got != 1 {
		t.Fatalf("window length after expiry = %d, want 1 current request", got)
	}
}

func TestWave2RateLimiterConcurrentAdmissionIsExact(t *testing.T) {
	rl := NewRateLimiter(60)
	t.Cleanup(rl.Stop)
	const callers = 256
	start := make(chan struct{})
	var admitted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if rl.Allow("198.51.100.30") {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := admitted.Load(); got != 60 {
		t.Fatalf("concurrent admissions = %d, want exactly 60", got)
	}
}

func TestWave2RateLimiterStopIsIdempotent(t *testing.T) {
	rl := NewRateLimiter(60)
	const stoppers = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(stoppers)
	for i := 0; i < stoppers; i++ {
		go func() {
			defer wg.Done()
			<-start
			rl.Stop()
		}()
	}
	close(start)
	wg.Wait()
	select {
	case <-rl.stop:
	default:
		t.Fatal("concurrent Stop calls did not close the stop channel")
	}
}

func TestWave2RateLimiterEvictsExpiredClients(t *testing.T) {
	now := time.Now()
	current := now.Add(-30 * time.Second)
	rl := &RateLimiter{
		windows: map[string][]time.Time{
			"expired": {now.Add(-2 * time.Minute)},
			"mixed":   {now.Add(-2 * time.Minute), current},
		},
		interval: time.Minute,
		stop:     make(chan struct{}),
	}

	rl.evictExpired(now)
	if _, exists := rl.windows["expired"]; exists {
		t.Fatal("fully expired client remained in limiter map")
	}
	if got := rl.windows["mixed"]; len(got) != 1 || !got[0].Equal(current) {
		t.Fatalf("mixed window after eviction = %v, want only %v", got, current)
	}
}

func TestWave2NewRateLimiterNormalizesNonPositiveLimit(t *testing.T) {
	rl := NewRateLimiter(-1)
	t.Cleanup(rl.Stop)
	if rl.limit != config.DefaultRateLimit {
		t.Fatalf("limiter limit = %d, want default %d",
			rl.limit, config.DefaultRateLimit)
	}
}

func TestWave2StartAPIServerRefusesNilRateLimiter(t *testing.T) {
	previousCfg := cfg
	previousFleet := fleetMgr
	previousServer := apiServer
	previousShutdownCtx := shutdownCtx
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if apiServer != nil && apiServer != previousServer {
			cleanupCtx, cleanupCancel := context.WithTimeout(
				context.Background(), time.Second)
			defer cleanupCancel()
			_ = apiServer.Shutdown(cleanupCtx)
		}
		cfg = previousCfg
		fleetMgr = previousFleet
		apiServer = previousServer
		shutdownCtx = previousShutdownCtx
	})

	cfg = &config.Config{API: config.APIConfig{ListenAddr: "127.0.0.1:0"}}
	fleetMgr = fleet.NewManager(cfg)
	apiServer = nil
	shutdownCtx = ctx
	startAPIServer(nil)
	if apiServer != nil {
		t.Fatal("API server started without a rate limiter")
	}
}

func TestWave2WireRouterEnforcesLimiterAtRequest61(t *testing.T) {
	testCfg := testConfig()
	pool, err := pgxpool.New(context.Background(),
		"postgres://test:test@127.0.0.1:1/test?sslmode=disable")
	if err != nil {
		t.Fatalf("create lazy test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	rl := NewRateLimiter(60)
	t.Cleanup(rl.Stop)
	result := wireRouter(WireParams{
		Cfg:         testCfg,
		Pool:        pool,
		FleetMgr:    testFleetMgr(testCfg),
		RateLimiter: rl,
	})

	for requestNo := 1; requestNo <= 61; requestNo++ {
		req := httptest.NewRequest(
			http.MethodGet, "/api/v1/auth/oauth/config", nil)
		req.RemoteAddr = "198.51.100.40:4321"
		rec := httptest.NewRecorder()
		result.Handler.ServeHTTP(rec, req)
		if requestNo <= 60 && rec.Code != http.StatusOK {
			t.Fatalf("wired request %d status = %d, want 200",
				requestNo, rec.Code)
		}
		if requestNo <= 60 &&
			!strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Fatalf("wired request %d did not reach OAuth config handler: %s",
				requestNo, rec.Body.String())
		}
		if requestNo == 61 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("wired request 61 status = %d, want 429", rec.Code)
		}
	}
}

func TestWave2WireRouterLimiterRunsBeforeAuth(t *testing.T) {
	testCfg := testConfig()
	rl := NewRateLimiter(1)
	t.Cleanup(rl.Stop)
	const client = "198.51.100.41"
	if !rl.Allow(client) {
		t.Fatal("failed to exhaust limiter before request")
	}
	result := wireRouter(WireParams{
		Cfg:         testCfg,
		FleetMgr:    testFleetMgr(testCfg),
		RateLimiter: rl,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.RemoteAddr = client + ":4321"
	rec := httptest.NewRecorder()
	result.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted unauthenticated request status = %d, want 429",
			rec.Code)
	}
}

func snapshotTrustedProxyNets() func() {
	trustedProxyNetsMu.RLock()
	previous := append([]*net.IPNet(nil), trustedProxyNets...)
	trustedProxyNetsMu.RUnlock()
	return func() {
		trustedProxyNetsMu.Lock()
		trustedProxyNets = previous
		trustedProxyNetsMu.Unlock()
	}
}
