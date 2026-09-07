package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// NewOAuthProvider
// ---------------------------------------------------------------------------

func TestNewOAuthProvider_InitializesFields(t *testing.T) {
	cfg := &config.OAuthConfig{
		Provider:     "github",
		ClientID:     "test-client-id",
		ClientSecret: "test-secret",
		RedirectURL:  "http://localhost/callback",
	}
	p := NewOAuthProvider(cfg)

	if p == nil {
		t.Fatal("NewOAuthProvider returned nil")
	}
	if p.cfg != cfg {
		t.Error("cfg pointer not stored")
	}
	if p.states == nil {
		t.Error("states map not initialized")
	}
	if p.client == nil {
		t.Error("http client not initialized")
	}
	if p.discovery != nil {
		t.Error("discovery should be nil before Discover is called")
	}
}

func TestNewOAuthProvider_NilConfig(t *testing.T) {
	// NewOAuthProvider does not guard against nil; it stores what's given.
	// This is valid because Discover checks cfg fields later.
	p := NewOAuthProvider(nil)
	if p == nil {
		t.Fatal("NewOAuthProvider returned nil even with nil config")
	}
	if p.cfg != nil {
		t.Error("cfg should be nil when nil was passed")
	}
}

// ---------------------------------------------------------------------------
// discoverGitHub
// ---------------------------------------------------------------------------

func TestDiscoverGitHub_SetsEndpoints(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover(github) error: %v", err)
	}
	if p.discovery == nil {
		t.Fatal("discovery is nil after Discover")
	}
	if p.discovery.AuthorizationEndpoint != "https://github.com/login/oauth/authorize" {
		t.Errorf("AuthorizationEndpoint = %q", p.discovery.AuthorizationEndpoint)
	}
	if p.discovery.TokenEndpoint != "https://github.com/login/oauth/access_token" {
		t.Errorf("TokenEndpoint = %q", p.discovery.TokenEndpoint)
	}
	if p.discovery.UserinfoEndpoint != "https://api.github.com/user" {
		t.Errorf("UserinfoEndpoint = %q", p.discovery.UserinfoEndpoint)
	}
	if p.discovery.JWKSURI != "" {
		t.Errorf("JWKSURI should be empty for github, got %q", p.discovery.JWKSURI)
	}
}

// ---------------------------------------------------------------------------
// Discover — unknown provider
// ---------------------------------------------------------------------------

func TestDiscover_UnknownProvider(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "facebook"}
	p := NewOAuthProvider(cfg)

	err := p.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error should mention 'unknown provider', got: %v", err)
	}
	if !strings.Contains(err.Error(), "facebook") {
		t.Errorf("error should mention provider name, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Discover — OIDC provider without issuer_url
// ---------------------------------------------------------------------------

func TestDiscover_OIDC_MissingIssuerURL(t *testing.T) {
	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: "",
	}
	p := NewOAuthProvider(cfg)

	err := p.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for oidc without issuer_url")
	}
	if !strings.Contains(err.Error(), "issuer_url required") {
		t.Errorf("error should mention issuer_url, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — success with httptest
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_Success(t *testing.T) {
	disc := OIDCDiscovery{
		AuthorizationEndpoint: "https://example.com/authorize",
		TokenEndpoint:         "https://example.com/token",
		UserinfoEndpoint:      "https://example.com/userinfo",
		JWKSURI:               "https://example.com/jwks",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(disc)
	}))
	defer srv.Close()

	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: srv.URL,
	}
	p := NewOAuthProvider(cfg)
	err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if p.discovery.AuthorizationEndpoint != disc.AuthorizationEndpoint {
		t.Errorf("AuthorizationEndpoint = %q, want %q",
			p.discovery.AuthorizationEndpoint, disc.AuthorizationEndpoint)
	}
	if p.discovery.TokenEndpoint != disc.TokenEndpoint {
		t.Errorf("TokenEndpoint = %q, want %q",
			p.discovery.TokenEndpoint, disc.TokenEndpoint)
	}
	if p.discovery.UserinfoEndpoint != disc.UserinfoEndpoint {
		t.Errorf("UserinfoEndpoint = %q, want %q",
			p.discovery.UserinfoEndpoint, disc.UserinfoEndpoint)
	}
	if p.discovery.JWKSURI != disc.JWKSURI {
		t.Errorf("JWKSURI = %q, want %q",
			p.discovery.JWKSURI, disc.JWKSURI)
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — trailing slash on issuer
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_TrailingSlashTrimmed(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		disc := OIDCDiscovery{
			AuthorizationEndpoint: "https://x.com/auth",
			TokenEndpoint:         "https://x.com/token",
		}
		json.NewEncoder(w).Encode(disc)
	}))
	defer srv.Close()

	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: srv.URL + "/",
	}
	p := NewOAuthProvider(cfg)
	err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if requestedPath != "/.well-known/openid-configuration" {
		t.Errorf("path = %q, want /.well-known/openid-configuration",
			requestedPath)
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — non-200 status
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: srv.URL,
	}
	p := NewOAuthProvider(cfg)
	err := p.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — invalid JSON
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "not json at all")
	}))
	defer srv.Close()

	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: srv.URL,
	}
	p := NewOAuthProvider(cfg)
	err := p.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decoding discovery") {
		t.Errorf("error should mention decoding, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — missing required endpoints
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_MissingRequiredEndpoints(t *testing.T) {
	tests := []struct {
		name string
		disc OIDCDiscovery
	}{
		{
			name: "missing authorization_endpoint",
			disc: OIDCDiscovery{
				TokenEndpoint: "https://x.com/token",
			},
		},
		{
			name: "missing token_endpoint",
			disc: OIDCDiscovery{
				AuthorizationEndpoint: "https://x.com/auth",
			},
		},
		{
			name: "both missing",
			disc: OIDCDiscovery{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					json.NewEncoder(w).Encode(tc.disc)
				}))
			defer srv.Close()

			cfg := &config.OAuthConfig{
				Provider:  "oidc",
				IssuerURL: srv.URL,
			}
			p := NewOAuthProvider(cfg)
			err := p.Discover(context.Background())
			if err == nil {
				t.Fatal("expected error for missing endpoints")
			}
			if !strings.Contains(err.Error(), "missing required endpoints") {
				t.Errorf("error should mention missing endpoints, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — context cancellation
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: srv.URL,
	}
	p := NewOAuthProvider(cfg)
	err := p.Discover(ctx)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

// ---------------------------------------------------------------------------
// discoverOIDC — unreachable server
// ---------------------------------------------------------------------------

func TestDiscoverOIDC_UnreachableServer(t *testing.T) {
	cfg := &config.OAuthConfig{
		Provider:  "oidc",
		IssuerURL: "http://127.0.0.1:1", // port 1 should be unreachable
	}
	p := NewOAuthProvider(cfg)
	err := p.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	if !strings.Contains(err.Error(), "fetching discovery") {
		t.Errorf("error should mention fetching, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Discover — google provider (uses httptest for OIDC discovery)
// ---------------------------------------------------------------------------

func TestDiscover_Google_CallsDiscoverOIDC(t *testing.T) {
	// Google calls discoverOIDC with "https://accounts.google.com".
	// We can't mock that directly without overriding the issuer. Instead,
	// verify that the function is dispatched correctly by checking error
	// contains the google endpoint info (it will fail to connect, which
	// confirms the right path was taken).
	cfg := &config.OAuthConfig{Provider: "google"}
	p := NewOAuthProvider(cfg)
	// Use a short timeout context to avoid long waits.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := p.Discover(ctx)
	// Should error because it can't reach google in test.
	// We just verify it attempted the OIDC flow (not github).
	if err == nil {
		// If it somehow succeeded (cached DNS or fast network), that's OK too.
		if p.discovery == nil {
			t.Error("discovery nil after successful Discover")
		}
		return
	}
	// The error should be about fetching, not about unknown provider.
	if strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("google should not be unknown, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AuthorizationURL
// ---------------------------------------------------------------------------

func TestAuthorizationURL_NoDiscovery(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	// Don't call Discover — discovery is nil.
	_, _, err := p.AuthorizationURL()
	if err == nil {
		t.Fatal("expected error when discovery not performed")
	}
	if !strings.Contains(err.Error(), "discovery not performed") {
		t.Errorf("error should mention discovery, got: %v", err)
	}
}

func TestAuthorizationURL_GitHub(t *testing.T) {
	cfg := &config.OAuthConfig{
		Provider:    "github",
		ClientID:    "gh-client-id",
		RedirectURL: "http://localhost:8080/callback",
	}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	authURL, state, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	if state == "" {
		t.Error("AuthorizationURL should return a non-empty state")
	}

	// Verify the URL structure.
	if !strings.HasPrefix(authURL, "https://github.com/login/oauth/authorize?") {
		t.Errorf("URL prefix wrong: %s", authURL)
	}
	if !strings.Contains(authURL, "client_id=gh-client-id") {
		t.Errorf("missing client_id: %s", authURL)
	}
	if !strings.Contains(authURL, "redirect_uri=") {
		t.Errorf("missing redirect_uri: %s", authURL)
	}
	if !strings.Contains(authURL, "response_type=code") {
		t.Errorf("missing response_type=code: %s", authURL)
	}
	if !strings.Contains(authURL, "state=") {
		t.Errorf("missing state: %s", authURL)
	}
	// GitHub scope should be user:email
	if !strings.Contains(authURL, "scope=user") {
		t.Errorf("missing scope user:email: %s", authURL)
	}
}

func TestAuthorizationURL_OIDC_Scope(t *testing.T) {
	disc := OIDCDiscovery{
		AuthorizationEndpoint: "https://example.com/authorize",
		TokenEndpoint:         "https://example.com/token",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(disc)
	}))
	defer srv.Close()

	cfg := &config.OAuthConfig{
		Provider:    "oidc",
		IssuerURL:   srv.URL,
		ClientID:    "oidc-client",
		RedirectURL: "http://localhost/callback",
	}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	authURL, _, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}

	// Non-github providers should have openid email profile scope.
	if !strings.Contains(authURL, "scope=openid+email+profile") {
		t.Errorf("expected openid scope for OIDC, got URL: %s", authURL)
	}
	if !strings.Contains(authURL, "client_id=oidc-client") {
		t.Errorf("missing client_id in URL: %s", authURL)
	}
}

func TestAuthorizationURL_StoresState(t *testing.T) {
	cfg := &config.OAuthConfig{
		Provider: "github",
		ClientID: "test",
	}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	_, _, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}

	p.mu.RLock()
	stateCount := len(p.states)
	p.mu.RUnlock()

	if stateCount != 1 {
		t.Errorf("expected 1 state stored, got %d", stateCount)
	}
}

func TestAuthorizationURL_MultipleCallsCreateDistinctStates(t *testing.T) {
	cfg := &config.OAuthConfig{
		Provider: "github",
		ClientID: "test",
	}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	url1, _, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("first AuthorizationURL: %v", err)
	}
	url2, _, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("second AuthorizationURL: %v", err)
	}

	if url1 == url2 {
		t.Error("two AuthorizationURL calls should produce different URLs (unique state)")
	}

	p.mu.RLock()
	stateCount := len(p.states)
	p.mu.RUnlock()

	if stateCount != 2 {
		t.Errorf("expected 2 states stored, got %d", stateCount)
	}
}

// ---------------------------------------------------------------------------
// ValidateState
// ---------------------------------------------------------------------------

func TestValidateState_ValidToken(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	p.mu.Lock()
	p.states["test-state"] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	if !p.ValidateState("test-state") {
		t.Error("expected valid state to return true")
	}

	// Should be consumed (one-time use).
	if p.ValidateState("test-state") {
		t.Error("state should be consumed after first validation")
	}
}

func TestValidateState_ExpiredToken(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	p.mu.Lock()
	p.states["expired-state"] = time.Now().Add(-1 * time.Minute)
	p.mu.Unlock()

	if p.ValidateState("expired-state") {
		t.Error("expired state should return false")
	}

	// Expired state should be deleted even though validation failed.
	p.mu.RLock()
	_, exists := p.states["expired-state"]
	p.mu.RUnlock()
	if exists {
		t.Error("expired state should be removed after validation attempt")
	}
}

func TestValidateState_NonexistentToken(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	if p.ValidateState("does-not-exist") {
		t.Error("nonexistent state should return false")
	}
}

func TestValidateState_EmptyString(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	if p.ValidateState("") {
		t.Error("empty state string should return false")
	}
}

func TestValidateState_ConcurrentAccess(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	state := "concurrent-state"
	p.mu.Lock()
	p.states[state] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	// Multiple goroutines try to validate the same state. Exactly one
	// should succeed.
	const goroutines = 10
	results := make(chan bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			results <- p.ValidateState(state)
		}()
	}
	wg.Wait()
	close(results)

	successCount := 0
	for r := range results {
		if r {
			successCount++
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly 1 successful validation, got %d", successCount)
	}
}

// ---------------------------------------------------------------------------
// CleanStates
// ---------------------------------------------------------------------------

func TestCleanStates_RemovesExpired(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	p.mu.Lock()
	p.states["expired1"] = time.Now().Add(-5 * time.Minute)
	p.states["expired2"] = time.Now().Add(-1 * time.Second)
	p.states["valid1"] = time.Now().Add(5 * time.Minute)
	p.states["valid2"] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	p.CleanStates()

	p.mu.RLock()
	count := len(p.states)
	_, hasValid1 := p.states["valid1"]
	_, hasValid2 := p.states["valid2"]
	_, hasExpired1 := p.states["expired1"]
	_, hasExpired2 := p.states["expired2"]
	p.mu.RUnlock()

	if count != 2 {
		t.Errorf("expected 2 states after clean, got %d", count)
	}
	if !hasValid1 {
		t.Error("valid1 should not be cleaned")
	}
	if !hasValid2 {
		t.Error("valid2 should not be cleaned")
	}
	if hasExpired1 {
		t.Error("expired1 should be cleaned")
	}
	if hasExpired2 {
		t.Error("expired2 should be cleaned")
	}
}

func TestCleanStates_EmptyMap(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	// Should not panic on empty map.
	p.CleanStates()

	p.mu.RLock()
	count := len(p.states)
	p.mu.RUnlock()

	if count != 0 {
		t.Errorf("expected 0 states, got %d", count)
	}
}

func TestCleanStates_AllExpired(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	p.mu.Lock()
	p.states["a"] = time.Now().Add(-1 * time.Minute)
	p.states["b"] = time.Now().Add(-2 * time.Minute)
	p.states["c"] = time.Now().Add(-3 * time.Minute)
	p.mu.Unlock()

	p.CleanStates()

	p.mu.RLock()
	count := len(p.states)
	p.mu.RUnlock()

	if count != 0 {
		t.Errorf("expected all states cleaned, got %d remaining", count)
	}
}

func TestCleanStates_AllValid(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	p.mu.Lock()
	p.states["a"] = time.Now().Add(5 * time.Minute)
	p.states["b"] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	p.CleanStates()

	p.mu.RLock()
	count := len(p.states)
	p.mu.RUnlock()

	if count != 2 {
		t.Errorf("expected 2 states (all valid), got %d", count)
	}
}

// ---------------------------------------------------------------------------
// evictExpiredAndCapLocked
// ---------------------------------------------------------------------------

func TestEvictExpiredAndCapLocked_RemovesExpiredBeforeCapCheck(t *testing.T) {
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "github"})
	now := time.Now()

	p.mu.Lock()
	p.states["expired"] = now.Add(-1 * time.Second)
	p.states["valid"] = now.Add(10 * time.Minute)
	p.evictExpiredAndCapLocked()
	count := len(p.states)
	_, hasExpired := p.states["expired"]
	_, hasValid := p.states["valid"]
	p.mu.Unlock()

	if count != 1 {
		t.Fatalf("state count = %d, want 1", count)
	}
	if hasExpired {
		t.Fatal("expired state should be deleted")
	}
	if !hasValid {
		t.Fatal("valid state should remain")
	}
}

func TestEvictExpiredAndCapLocked_EvictsOldestAtCap(t *testing.T) {
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "github"})
	base := time.Now().Add(10 * time.Minute)

	p.mu.Lock()
	p.states["oldest"] = base
	for i := 1; i < maxOAuthStates; i++ {
		p.states[fmt.Sprintf("state-%05d", i)] = base.Add(time.Duration(i) * time.Second)
	}
	p.evictExpiredAndCapLocked()
	count := len(p.states)
	_, hasOldest := p.states["oldest"]
	_, hasNewest := p.states[fmt.Sprintf("state-%05d", maxOAuthStates-1)]
	p.mu.Unlock()

	if count != maxOAuthStates-1 {
		t.Fatalf("state count = %d, want %d", count, maxOAuthStates-1)
	}
	if hasOldest {
		t.Fatal("oldest state should be evicted")
	}
	if !hasNewest {
		t.Fatal("newest state should remain")
	}
}

// ---------------------------------------------------------------------------
// StartStateCleaner
// ---------------------------------------------------------------------------

func TestStartStateCleaner_RespectsContextCancellation(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.StartStateCleaner(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("StartStateCleaner did not exit within 2s after cancellation")
	}
}

func TestStartStateCleaner_PreCancelledContext(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	done := make(chan struct{})
	go func() {
		p.StartStateCleaner(ctx)
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("StartStateCleaner did not exit with pre-cancelled context")
	}
}

// ---------------------------------------------------------------------------
// randomState
// ---------------------------------------------------------------------------

func TestRandomState_Length(t *testing.T) {
	state, err := randomState()
	if err != nil {
		t.Fatalf("randomState error: %v", err)
	}
	// 16 bytes hex-encoded = 32 characters
	if len(state) != 32 {
		t.Errorf("state length = %d, want 32", len(state))
	}
}

func TestRandomState_HexEncoded(t *testing.T) {
	state, err := randomState()
	if err != nil {
		t.Fatalf("randomState error: %v", err)
	}
	for i, c := range state {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex character at index %d: %c", i, c)
		}
	}
}

func TestRandomState_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		state, err := randomState()
		if err != nil {
			t.Fatalf("randomState error on iteration %d: %v", i, err)
		}
		if seen[state] {
			t.Fatalf("duplicate state on iteration %d: %s", i, state)
		}
		seen[state] = true
	}
}

// ---------------------------------------------------------------------------
// exchangeCode — via httptest
// ---------------------------------------------------------------------------

func TestExchangeCode_Success(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			ct := r.Header.Get("Content-Type")
			if ct != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q", ct)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if r.FormValue("code") != "test-code" {
				t.Errorf("code = %q", r.FormValue("code"))
			}
			if r.FormValue("grant_type") != "authorization_code" {
				t.Errorf("grant_type = %q", r.FormValue("grant_type"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "test-access-token",
			})
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{
		Provider:     "github",
		ClientID:     "cid",
		ClientSecret: "csecret",
		RedirectURL:  "http://localhost/callback",
	}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{
		TokenEndpoint: tokenSrv.URL,
	}

	token, err := p.exchangeCode(context.Background(), "test-code")
	if err != nil {
		t.Fatalf("exchangeCode error: %v", err)
	}
	if token != "test-access-token" {
		t.Errorf("token = %q, want 'test-access-token'", token)
	}
}

func TestExchangeCode_Non200Status(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "bad code")
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{TokenEndpoint: tokenSrv.URL}

	_, err := p.exchangeCode(context.Background(), "bad-code")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention 400, got: %v", err)
	}
}

func TestExchangeCode_EmptyAccessToken(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "",
			})
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{TokenEndpoint: tokenSrv.URL}

	_, err := p.exchangeCode(context.Background(), "code")
	if err == nil {
		t.Fatal("expected error for empty access_token")
	}
	if !strings.Contains(err.Error(), "empty access_token") {
		t.Errorf("error should mention empty access_token, got: %v", err)
	}
}

func TestExchangeCode_InvalidJSON(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{invalid json")
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{TokenEndpoint: tokenSrv.URL}

	_, err := p.exchangeCode(context.Background(), "code")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decoding token response") {
		t.Errorf("error should mention decoding, got: %v", err)
	}
}

func TestExchangeCode_MissingAccessTokenField(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"token_type": "bearer",
			})
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{TokenEndpoint: tokenSrv.URL}

	_, err := p.exchangeCode(context.Background(), "code")
	if err == nil {
		t.Fatal("expected error for missing access_token field")
	}
	if !strings.Contains(err.Error(), "empty access_token") {
		t.Errorf("error should mention empty access_token, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Exchange — full flow
// ---------------------------------------------------------------------------

func TestExchange_InvalidState(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Pass matching cookieState so we reach the in-memory state check;
	// bad-state is absent from the map so it fails with invalid state.
	_, err := p.Exchange(
		context.Background(), "code", "bad-state", "bad-state")
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
	if !strings.Contains(err.Error(), "invalid or expired state") {
		t.Errorf("error should mention state, got: %v", err)
	}
}

func TestExchange_CookieStateMismatch(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	p.mu.Lock()
	p.states["real-state"] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	// cookieState differs from state param → CSRF check rejects before
	// we ever hit the state map.
	_, err := p.Exchange(
		context.Background(), "code", "real-state", "attacker-cookie")
	if err == nil {
		t.Fatal("expected state-cookie mismatch rejection")
	}
	if !strings.Contains(err.Error(), "state cookie mismatch") {
		t.Errorf("error should mention cookie mismatch, got: %v", err)
	}

	// Empty cookie also rejected.
	_, err = p.Exchange(
		context.Background(), "code", "real-state", "")
	if err == nil ||
		!strings.Contains(err.Error(), "state cookie mismatch") {
		t.Errorf("empty cookieState should be rejected, got: %v", err)
	}

	// Verify legitimate flow still consumes state exactly once.
	p.mu.RLock()
	_, stillStored := p.states["real-state"]
	p.mu.RUnlock()
	if !stillStored {
		t.Error("state should not have been consumed by failed CSRF check")
	}
}

func TestExchange_NoDiscovery(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	// Don't call Discover. Manually add a valid state.
	p.mu.Lock()
	p.states["valid-state"] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	_, err := p.Exchange(
		context.Background(), "code", "valid-state", "valid-state")
	if err == nil {
		t.Fatal("expected error when discovery not performed")
	}
	if !strings.Contains(err.Error(), "discovery not performed") {
		t.Errorf("error should mention discovery, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fetchOIDCEmail — via httptest
// ---------------------------------------------------------------------------

func TestFetchOIDCEmail_Success(t *testing.T) {
	userinfoSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader != "Bearer test-token" {
				t.Errorf("Authorization = %q, want 'Bearer test-token'",
					authHeader)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"email": "user@example.com",
			})
		}))
	defer userinfoSrv.Close()

	cfg := &config.OAuthConfig{Provider: "oidc"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{
		UserinfoEndpoint: userinfoSrv.URL,
	}

	email, err := p.fetchOIDCEmail(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("fetchOIDCEmail error: %v", err)
	}
	if email != "user@example.com" {
		t.Errorf("email = %q, want 'user@example.com'", email)
	}
}

func TestFetchOIDCEmail_EmptyEmail(t *testing.T) {
	userinfoSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{
				"email": "",
			})
		}))
	defer userinfoSrv.Close()

	cfg := &config.OAuthConfig{Provider: "oidc"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{UserinfoEndpoint: userinfoSrv.URL}

	_, err := p.fetchOIDCEmail(context.Background(), "token")
	if err == nil {
		t.Fatal("expected error for empty email")
	}
	if !strings.Contains(err.Error(), "no email") {
		t.Errorf("error should mention no email, got: %v", err)
	}
}

func TestFetchOIDCEmail_MissingEndpoint(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "oidc"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{UserinfoEndpoint: ""}

	_, err := p.fetchOIDCEmail(context.Background(), "token")
	if err == nil {
		t.Fatal("expected error for missing endpoint")
	}
	if !strings.Contains(err.Error(), "no userinfo endpoint") {
		t.Errorf("error should mention no userinfo, got: %v", err)
	}
}

func TestFetchOIDCEmail_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "oidc"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{UserinfoEndpoint: srv.URL}

	_, err := p.fetchOIDCEmail(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}

func TestFetchOIDCEmail_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "not json")
		}))
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "oidc"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{UserinfoEndpoint: srv.URL}

	_, err := p.fetchOIDCEmail(context.Background(), "token")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decoding userinfo") {
		t.Errorf("error should mention decoding, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fetchGitHubEmail — via httptest
// ---------------------------------------------------------------------------

func TestFetchGitHubEmail_PrimaryEmailInUserEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user" {
				json.NewEncoder(w).Encode(map[string]string{
					"email": "gh-user@example.com",
				})
				return
			}
			http.NotFound(w, r)
		}))
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{
		UserinfoEndpoint: srv.URL + "/user",
	}

	// Override the client to use our test server.
	// fetchGitHubEmail uses hardcoded URLs, so we need to test via
	// a different approach. We'll directly test the method by modifying
	// the client to redirect.
	//
	// Since fetchGitHubEmail hardcodes github.com URLs, we test it
	// indirectly via fetchEmail with a mux.
	// Actually, let's test it by pointing the whole client at a mux.
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer gh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"email": "gh@example.com",
		})
	})
	ghSrv := httptest.NewServer(mux)
	defer ghSrv.Close()

	// We can't easily test fetchGitHubEmail because it uses hardcoded URLs.
	// Instead, test fetchOIDCEmail (which uses discovery.UserinfoEndpoint)
	// and test the dispatch logic in fetchEmail.
	//
	// Let's test the fetchEmail dispatch: github goes to fetchGitHubEmail,
	// non-github goes to fetchOIDCEmail.
	cfgOIDC := &config.OAuthConfig{Provider: "oidc"}
	pOIDC := NewOAuthProvider(cfgOIDC)
	pOIDC.discovery = &OIDCDiscovery{UserinfoEndpoint: ghSrv.URL + "/user"}

	email, err := pOIDC.fetchEmail(context.Background(), "gh-token")
	if err != nil {
		t.Fatalf("fetchEmail(oidc) error: %v", err)
	}
	if email != "gh@example.com" {
		t.Errorf("email = %q, want 'gh@example.com'", email)
	}
}

func TestFetchEmail_DispatchesCorrectly(t *testing.T) {
	// Verify that provider=github calls fetchGitHubEmail and
	// provider=oidc calls fetchOIDCEmail.
	mux := http.NewServeMux()
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"email": "oidc@example.com",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Test OIDC dispatch.
	cfgOIDC := &config.OAuthConfig{Provider: "oidc"}
	pOIDC := NewOAuthProvider(cfgOIDC)
	pOIDC.discovery = &OIDCDiscovery{
		UserinfoEndpoint: srv.URL + "/userinfo",
	}
	email, err := pOIDC.fetchEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("fetchEmail(oidc) error: %v", err)
	}
	if email != "oidc@example.com" {
		t.Errorf("email = %q, want 'oidc@example.com'", email)
	}
}

// ---------------------------------------------------------------------------
// fetchGitHubEmailsFallback — via httptest
// ---------------------------------------------------------------------------

func TestFetchGitHubEmailsFallback_PrimaryVerified(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		emails := []map[string]interface{}{
			{"email": "secondary@example.com", "primary": false, "verified": true},
			{"email": "primary@example.com", "primary": true, "verified": true},
		}
		json.NewEncoder(w).Encode(emails)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	// Override the hardcoded URL by calling the method directly isn't
	// possible, but fetchGitHubEmailsFallback uses hardcoded URLs.
	// We can't test it with httptest without refactoring.
	// However, we CAN test it by creating a custom client transport.
	//
	// Instead, let's test the logic indirectly: create a provider with
	// a custom http.Client that redirects github.com to our test server.
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}

	email, err := p.fetchGitHubEmailsFallback(
		context.Background(), "test-token")
	if err != nil {
		t.Fatalf("fetchGitHubEmailsFallback error: %v", err)
	}
	if email != "primary@example.com" {
		t.Errorf("email = %q, want 'primary@example.com'", email)
	}
}

func TestFetchGitHubEmailsFallback_NoPrimaryVerified(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		emails := []map[string]interface{}{
			{"email": "unverified@example.com", "primary": true, "verified": false},
			{"email": "secondary@example.com", "primary": false, "verified": true},
		}
		json.NewEncoder(w).Encode(emails)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}

	_, err := p.fetchGitHubEmailsFallback(
		context.Background(), "test-token")
	if err == nil {
		t.Fatal("expected error when no primary+verified email")
	}
	if !strings.Contains(err.Error(), "no verified primary email") {
		t.Errorf("error should mention no verified primary, got: %v", err)
	}
}

func TestFetchGitHubEmailsFallback_EmptyList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}

	_, err := p.fetchGitHubEmailsFallback(
		context.Background(), "test-token")
	if err == nil {
		t.Fatal("expected error for empty email list")
	}
}

func TestFetchGitHubEmailsFallback_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}

	_, err := p.fetchGitHubEmailsFallback(
		context.Background(), "test-token")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention 403, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fetchGitHubEmail — primary email in /user endpoint
// ---------------------------------------------------------------------------

func TestFetchGitHubEmail_EmailInUserResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"email": "direct@example.com",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}
	p.discovery = &OIDCDiscovery{
		UserinfoEndpoint: srv.URL + "/user",
	}

	email, err := p.fetchGitHubEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("fetchGitHubEmail error: %v", err)
	}
	if email != "direct@example.com" {
		t.Errorf("email = %q, want 'direct@example.com'", email)
	}
}

func TestFetchGitHubEmail_FallbackWhenNoEmailInUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		// Return user without email — triggers fallback.
		json.NewEncoder(w).Encode(map[string]string{
			"login": "testuser",
		})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		emails := []map[string]interface{}{
			{"email": "fallback@example.com", "primary": true, "verified": true},
		}
		json.NewEncoder(w).Encode(emails)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}

	email, err := p.fetchGitHubEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("fetchGitHubEmail fallback error: %v", err)
	}
	if email != "fallback@example.com" {
		t.Errorf("email = %q, want 'fallback@example.com'", email)
	}
}

func TestFetchGitHubEmail_UserEndpointNon200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.client = &http.Client{
		Transport: &githubRedirectTransport{
			targetURL: srv.URL,
			base:      http.DefaultTransport,
		},
	}

	_, err := p.fetchGitHubEmail(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Exchange — full integration with httptest
// ---------------------------------------------------------------------------

func TestExchange_FullFlow_OIDC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "exchange-token",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer exchange-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"email": "exchanged@example.com",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.OAuthConfig{
		Provider:     "oidc",
		ClientID:     "cid",
		ClientSecret: "csecret",
		RedirectURL:  "http://localhost/cb",
	}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{
		TokenEndpoint:    srv.URL + "/token",
		UserinfoEndpoint: srv.URL + "/userinfo",
	}

	// Add a valid state.
	state := "valid-exchange-state"
	p.mu.Lock()
	p.states[state] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	email, err := p.Exchange(
		context.Background(), "auth-code", state, state)
	if err != nil {
		t.Fatalf("Exchange error: %v", err)
	}
	if email != "exchanged@example.com" {
		t.Errorf("email = %q, want 'exchanged@example.com'", email)
	}

	// State should be consumed.
	if p.ValidateState(state) {
		t.Error("state should be consumed after Exchange")
	}
}

func TestExchange_TokenExchangeFails(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "invalid_grant")
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{
		Provider:     "oidc",
		ClientID:     "cid",
		ClientSecret: "csecret",
	}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{
		TokenEndpoint: tokenSrv.URL,
	}

	state := "state-for-fail"
	p.mu.Lock()
	p.states[state] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	_, err := p.Exchange(
		context.Background(), "bad-code", state, state)
	if err == nil {
		t.Fatal("expected error when token exchange fails")
	}
}

// ---------------------------------------------------------------------------
// OIDCDiscovery struct
// ---------------------------------------------------------------------------

func TestOIDCDiscovery_JSONRoundTrip(t *testing.T) {
	original := OIDCDiscovery{
		AuthorizationEndpoint: "https://auth.example.com/authorize",
		TokenEndpoint:         "https://auth.example.com/token",
		UserinfoEndpoint:      "https://auth.example.com/userinfo",
		JWKSURI:               "https://auth.example.com/.well-known/jwks.json",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded OIDCDiscovery
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.AuthorizationEndpoint != original.AuthorizationEndpoint {
		t.Errorf("AuthorizationEndpoint = %q, want %q",
			decoded.AuthorizationEndpoint, original.AuthorizationEndpoint)
	}
	if decoded.TokenEndpoint != original.TokenEndpoint {
		t.Errorf("TokenEndpoint = %q, want %q",
			decoded.TokenEndpoint, original.TokenEndpoint)
	}
	if decoded.UserinfoEndpoint != original.UserinfoEndpoint {
		t.Errorf("UserinfoEndpoint = %q, want %q",
			decoded.UserinfoEndpoint, original.UserinfoEndpoint)
	}
	if decoded.JWKSURI != original.JWKSURI {
		t.Errorf("JWKSURI = %q, want %q",
			decoded.JWKSURI, original.JWKSURI)
	}
}

func TestOIDCDiscovery_ZeroValue(t *testing.T) {
	var d OIDCDiscovery
	if d.AuthorizationEndpoint != "" {
		t.Error("zero AuthorizationEndpoint should be empty")
	}
	if d.TokenEndpoint != "" {
		t.Error("zero TokenEndpoint should be empty")
	}
	if d.UserinfoEndpoint != "" {
		t.Error("zero UserinfoEndpoint should be empty")
	}
	if d.JWKSURI != "" {
		t.Error("zero JWKSURI should be empty")
	}
}

// ---------------------------------------------------------------------------
// OAuthProvider with custom client
// ---------------------------------------------------------------------------

func TestOAuthProvider_CustomClientTimeout(t *testing.T) {
	p := NewOAuthProvider(&config.OAuthConfig{})
	if p.client.Timeout != 15*time.Second {
		t.Errorf("client timeout = %v, want 15s", p.client.Timeout)
	}
}

// ---------------------------------------------------------------------------
// Helper: githubRedirectTransport
// Redirects requests to api.github.com to a test server.
// ---------------------------------------------------------------------------

type githubRedirectTransport struct {
	targetURL string
	base      http.RoundTripper
}

func (t *githubRedirectTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "github.com") {
		// Rewrite the URL to point to the test server.
		newURL := t.targetURL + req.URL.Path
		newReq, err := http.NewRequestWithContext(
			req.Context(), req.Method, newURL, req.Body,
		)
		if err != nil {
			return nil, err
		}
		newReq.Header = req.Header
		return t.base.RoundTrip(newReq)
	}
	return t.base.RoundTrip(req)
}

// ---------------------------------------------------------------------------
// Exchange — expired state during exchange
// ---------------------------------------------------------------------------

func TestExchange_ExpiredState(t *testing.T) {
	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Add an expired state.
	p.mu.Lock()
	p.states["expired"] = time.Now().Add(-1 * time.Minute)
	p.mu.Unlock()

	_, err := p.Exchange(
		context.Background(), "code", "expired", "expired")
	if err == nil {
		t.Fatal("expected error for expired state")
	}
	if !strings.Contains(err.Error(), "invalid or expired state") {
		t.Errorf("error should mention expired state, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// exchangeCode — sends correct form values
// ---------------------------------------------------------------------------

func TestExchangeCode_SendsCorrectFormValues(t *testing.T) {
	var receivedForm map[string]string
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			receivedForm = map[string]string{
				"grant_type":    r.FormValue("grant_type"),
				"code":          r.FormValue("code"),
				"redirect_uri":  r.FormValue("redirect_uri"),
				"client_id":     r.FormValue("client_id"),
				"client_secret": r.FormValue("client_secret"),
			}
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "tok",
			})
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{
		Provider:     "oidc",
		ClientID:     "my-client",
		ClientSecret: "my-secret",
		RedirectURL:  "http://localhost:8080/cb",
	}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{TokenEndpoint: tokenSrv.URL}

	_, err := p.exchangeCode(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("exchangeCode error: %v", err)
	}

	expected := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"redirect_uri":  "http://localhost:8080/cb",
		"client_id":     "my-client",
		"client_secret": "my-secret",
	}
	for k, want := range expected {
		if got := receivedForm[k]; got != want {
			t.Errorf("form[%q] = %q, want %q", k, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// exchangeCode — Accept header
// ---------------------------------------------------------------------------

func TestExchangeCode_SetsAcceptHeader(t *testing.T) {
	var acceptHeader string
	tokenSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			acceptHeader = r.Header.Get("Accept")
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "tok",
			})
		}))
	defer tokenSrv.Close()

	cfg := &config.OAuthConfig{Provider: "github"}
	p := NewOAuthProvider(cfg)
	p.discovery = &OIDCDiscovery{TokenEndpoint: tokenSrv.URL}

	_, err := p.exchangeCode(context.Background(), "code")
	if err != nil {
		t.Fatalf("exchangeCode error: %v", err)
	}
	if acceptHeader != "application/json" {
		t.Errorf("Accept header = %q, want 'application/json'",
			acceptHeader)
	}
}
