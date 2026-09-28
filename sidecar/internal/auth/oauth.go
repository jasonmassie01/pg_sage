package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	// links binds a pending state to the account it will link (D7). An
	// entry lives exactly as long as its state.
	links  map[string]LinkIntent
	client *http.Client
}

// NewOAuthProvider creates an OAuthProvider from configuration.
func NewOAuthProvider(
	cfg *config.OAuthConfig,
) *OAuthProvider {
	return &OAuthProvider{
		cfg:    cfg,
		states: make(map[string]time.Time),
		links:  make(map[string]LinkIntent),
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
	defer func() { _ = resp.Body.Close() }()

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
	return p.authorizationURL(LinkIntent{})
}

// AuthorizationURLForLink starts a round trip whose callback links the
// returned identity to userID instead of signing in. The caller must have
// established userID from a password session or a redeemed link grant.
func (p *OAuthProvider) AuthorizationURLForLink(
	userID int, via string,
) (string, string, error) {
	if userID <= 0 {
		return "", "", fmt.Errorf("oauth: link requires a user")
	}
	return p.authorizationURL(LinkIntent{UserID: userID, Via: via})
}

// LinkIntentForState returns the link intent bound to a pending state, or
// the zero intent for a plain login state or an unknown state.
func (p *OAuthProvider) LinkIntentForState(state string) LinkIntent {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.links[state]
}

func (p *OAuthProvider) authorizationURL(link LinkIntent) (string, string, error) {
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
	if link.UserID > 0 {
		if p.links == nil {
			p.links = make(map[string]LinkIntent)
		}
		p.links[state] = link
	}
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

// Exchange trades an authorization code for the caller's verified
// identity (issuer, subject, email). cookieState
// is the value of the oauth_state cookie set on the browser when the
// authorize step ran — it must equal the state query param for the
// callback to be accepted. This binds the state token to the
// originating browser and defeats login CSRF.
func (p *OAuthProvider) Exchange(
	ctx context.Context, code, state, cookieState string,
) (Identity, error) {
	// Constant-time-ish equality via plain compare is fine here: the
	// length of hex-encoded state is fixed, and leaking one bit via
	// timing does not help an attacker who must also know the random
	// state value itself.
	if cookieState == "" || cookieState != state {
		return Identity{}, fmt.Errorf("oauth: state cookie mismatch")
	}
	if !p.ValidateState(state) {
		return Identity{}, fmt.Errorf("oauth: invalid or expired state")
	}
	if p.discovery == nil {
		return Identity{}, fmt.Errorf("oauth: discovery not performed")
	}
	token, err := p.exchangeCode(ctx, code)
	if err != nil {
		return Identity{}, err
	}
	return p.fetchIdentity(ctx, token)
}

// ValidateState checks and consumes a CSRF state token.
func (p *OAuthProvider) ValidateState(state string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.states[state]
	if !ok {
		return false
	}
	p.dropStateLocked(state)
	return time.Now().Before(exp)
}

// CleanStates removes expired state tokens.
func (p *OAuthProvider) CleanStates() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, exp := range p.states {
		if now.After(exp) {
			p.dropStateLocked(k)
		}
	}
}

// dropStateLocked removes a state and any link intent bound to it.
func (p *OAuthProvider) dropStateLocked(state string) {
	delete(p.states, state)
	delete(p.links, state)
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
			p.dropStateLocked(k)
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
	p.dropStateLocked(oldestKey)
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
	defer func() { _ = resp.Body.Close() }()

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

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
