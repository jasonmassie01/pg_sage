package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// OIDCDiscovery holds endpoints from .well-known/openid-configuration.
type OIDCDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// OAuthProvider manages OAuth2/OIDC authentication flows.
type OAuthProvider struct {
	cfg       *config.OAuthConfig
	discovery *OIDCDiscovery
	mu        sync.RWMutex
	states    map[string]time.Time
	client    *http.Client
}

// NewOAuthProvider creates an OAuthProvider from configuration.
func NewOAuthProvider(
	cfg *config.OAuthConfig,
) *OAuthProvider {
	return &OAuthProvider{
		cfg:    cfg,
		states: make(map[string]time.Time),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Discover fetches OIDC metadata or sets well-known provider
// endpoints. Must be called before AuthorizationURL or Exchange.
func (p *OAuthProvider) Discover(
	ctx context.Context,
) error {
	switch p.cfg.Provider {
	case "github":
		return p.discoverGitHub()
	case "google":
		return p.discoverOIDC(ctx, "https://accounts.google.com")
	case "oidc":
		if p.cfg.IssuerURL == "" {
			return fmt.Errorf("oauth: issuer_url required for oidc provider")
		}
		return p.discoverOIDC(ctx, p.cfg.IssuerURL)
	default:
		return fmt.Errorf("oauth: unknown provider %q", p.cfg.Provider)
	}
}

func (p *OAuthProvider) discoverGitHub() error {
	p.discovery = &OIDCDiscovery{
		AuthorizationEndpoint: "https://github.com/login/oauth/authorize",
		TokenEndpoint:         "https://github.com/login/oauth/access_token",
		UserinfoEndpoint:      "https://api.github.com/user",
	}
	return nil
}

func (p *OAuthProvider) discoverOIDC(
	ctx context.Context, issuer string,
) error {
	wellKnown := strings.TrimRight(issuer, "/") +
		"/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, wellKnown, nil,
	)
	if err != nil {
		return fmt.Errorf("oauth: building discovery request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: fetching discovery doc: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"oauth: discovery returned status %d", resp.StatusCode,
		)
	}
	var disc OIDCDiscovery
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		return fmt.Errorf("oauth: decoding discovery doc: %w", err)
	}
	if disc.AuthorizationEndpoint == "" || disc.TokenEndpoint == "" {
		return fmt.Errorf("oauth: discovery missing required endpoints")
	}
	p.discovery = &disc
	return nil
}

// AuthorizationURL generates the redirect URL for user login and
// returns the CSRF state the caller must bind to the browser via an
// HttpOnly+Secure+SameSite=Lax cookie. On callback the cookie value
// MUST equal the state query parameter — this prevents login CSRF
// where an attacker forces a victim to sign in as the attacker.
func (p *OAuthProvider) AuthorizationURL() (string, string, error) {
	if p.discovery == nil {
		return "", "", fmt.Errorf("oauth: discovery not performed")
	}
	state, err := randomState()
	if err != nil {
		return "", "", fmt.Errorf("oauth: generating state: %w", err)
	}

	p.mu.Lock()
	p.evictExpiredAndCapLocked()
	p.states[state] = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	params := url.Values{
		"client_id":     {p.cfg.ClientID},
		"redirect_uri":  {p.cfg.RedirectURL},
		"response_type": {"code"},
		"state":         {state},
	}
	if p.cfg.Provider != "github" {
		params.Set("scope", "openid email profile")
	} else {
		params.Set("scope", "user:email")
	}
	return p.discovery.AuthorizationEndpoint + "?" +
		params.Encode(), state, nil
}

// Exchange trades an authorization code for user email. cookieState
// is the value of the oauth_state cookie set on the browser when the
// authorize step ran — it must equal the state query param for the
// callback to be accepted. This binds the state token to the
// originating browser and defeats login CSRF.
func (p *OAuthProvider) Exchange(
	ctx context.Context, code, state, cookieState string,
) (string, error) {
	// Constant-time-ish equality via plain compare is fine here: the
	// length of hex-encoded state is fixed, and leaking one bit via
	// timing does not help an attacker who must also know the random
	// state value itself.
	if cookieState == "" || cookieState != state {
		return "", fmt.Errorf("oauth: state cookie mismatch")
	}
	if !p.ValidateState(state) {
		return "", fmt.Errorf("oauth: invalid or expired state")
	}
	if p.discovery == nil {
		return "", fmt.Errorf("oauth: discovery not performed")
	}
	token, err := p.exchangeCode(ctx, code)
	if err != nil {
		return "", err
	}
	return p.fetchEmail(ctx, token)
}

// ValidateState checks and consumes a CSRF state token.
func (p *OAuthProvider) ValidateState(state string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.states[state]
	if !ok {
		return false
	}
	delete(p.states, state)
	return time.Now().Before(exp)
}

// CleanStates removes expired state tokens.
func (p *OAuthProvider) CleanStates() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, exp := range p.states {
		if now.After(exp) {
			delete(p.states, k)
		}
	}
}

// maxOAuthStates caps the pending-state map so that an attacker
// flooding the unauthenticated /auth/oauth/authorize endpoint
// cannot grow it without bound between cleanup ticks. Each
// state is tiny, but at ~10M pending states the memory cost
// becomes material. Chosen well above any plausible legitimate
// burst of concurrent logins.
const maxOAuthStates = 10_000

// evictExpiredAndCapLocked is called while holding p.mu before
// inserting a new state. It drops expired entries first, then if
// the map is still at the cap, evicts the entry with the earliest
// expiry time (closest to expiring naturally anyway).
func (p *OAuthProvider) evictExpiredAndCapLocked() {
	now := time.Now()
	for k, exp := range p.states {
		if now.After(exp) {
			delete(p.states, k)
		}
	}
	if len(p.states) < maxOAuthStates {
		return
	}
	var oldestKey string
	var oldestExp time.Time
	first := true
	for k, exp := range p.states {
		if first || exp.Before(oldestExp) {
			oldestKey = k
			oldestExp = exp
			first = false
		}
	}
	delete(p.states, oldestKey)
}

// StartStateCleaner periodically cleans expired CSRF tokens.
func (p *OAuthProvider) StartStateCleaner(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.CleanStates()
		}
	}
}

func (p *OAuthProvider) exchangeCode(
	ctx context.Context, code string,
) (string, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.discovery.TokenEndpoint,
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("oauth: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("oauth: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"oauth: token endpoint returned %d: %s",
			resp.StatusCode, string(body),
		)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("oauth: decoding token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("oauth: empty access_token in response")
	}
	return tokenResp.AccessToken, nil
}

func (p *OAuthProvider) fetchEmail(
	ctx context.Context, accessToken string,
) (string, error) {
	if p.cfg.Provider == "github" {
		return p.fetchGitHubEmail(ctx, accessToken)
	}
	return p.fetchOIDCEmail(ctx, accessToken)
}

func (p *OAuthProvider) fetchOIDCEmail(
	ctx context.Context, accessToken string,
) (string, error) {
	endpoint := p.discovery.UserinfoEndpoint
	if endpoint == "" {
		return "", fmt.Errorf("oauth: no userinfo endpoint")
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, endpoint, nil,
	)
	if err != nil {
		return "", fmt.Errorf("oauth: building userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: userinfo request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"oauth: userinfo returned status %d", resp.StatusCode,
		)
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("oauth: decoding userinfo: %w", err)
	}
	if info.Email == "" {
		return "", fmt.Errorf("oauth: no email in userinfo response")
	}
	slog.Info("oauth: authenticated user", "email", info.Email)
	return info.Email, nil
}

func (p *OAuthProvider) fetchGitHubEmail(
	ctx context.Context, accessToken string,
) (string, error) {
	// Try primary user endpoint first.
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		"https://api.github.com/user", nil,
	)
	if err != nil {
		return "", fmt.Errorf("oauth: building github user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: github user request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"oauth: github /user returned status %d", resp.StatusCode,
		)
	}
	var user struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", fmt.Errorf("oauth: decoding github user: %w", err)
	}
	if user.Email != "" {
		slog.Info("oauth: authenticated github user",
			"email", user.Email)
		return user.Email, nil
	}

	// Fallback: fetch from /user/emails endpoint.
	return p.fetchGitHubEmailsFallback(ctx, accessToken)
}

func (p *OAuthProvider) fetchGitHubEmailsFallback(
	ctx context.Context, accessToken string,
) (string, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		"https://api.github.com/user/emails", nil,
	)
	if err != nil {
		return "", fmt.Errorf(
			"oauth: building github emails request: %w", err,
		)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf(
			"oauth: github emails request failed: %w", err,
		)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"oauth: github /user/emails returned %d",
			resp.StatusCode,
		)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", fmt.Errorf(
			"oauth: decoding github emails: %w", err,
		)
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			slog.Info("oauth: authenticated github user",
				"email", e.Email)
			return e.Email, nil
		}
	}
	return "", fmt.Errorf("oauth: no verified primary email on github")
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
