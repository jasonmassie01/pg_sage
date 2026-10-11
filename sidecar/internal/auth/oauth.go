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

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/pg-sage/sidecar/internal/config"
)

// OIDCDiscovery holds endpoints from .well-known/openid-configuration.
type OIDCDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
}

// OAuthProvider manages OAuth2/OIDC authentication flows.
type OAuthProvider struct {
	cfg       *config.OAuthConfig
	discovery *OIDCDiscovery
	// verifier checks id_token signature, issuer, audience and expiry
	// (CG-03); nil until OIDC discovery succeeds.
	verifier *oidc.IDTokenVerifier
	mu       sync.RWMutex
	states   map[string]time.Time
	// links binds a pending state to the account it will link (D7). An
	// entry lives exactly as long as its state.
	links map[string]LinkIntent
	// pending holds each state's PKCE verifier and OIDC nonce; it lives
	// exactly as long as its state.
	pending map[string]pendingAuth
	client  *http.Client
}

// pendingAuth is the per-login secret material bound to one state.
type pendingAuth struct {
	codeVerifier string
	nonce        string
}

// NewOAuthProvider creates an OAuthProvider from configuration.
func NewOAuthProvider(
	cfg *config.OAuthConfig,
) *OAuthProvider {
	return &OAuthProvider{
		cfg:     cfg,
		states:  make(map[string]time.Time),
		links:   make(map[string]LinkIntent),
		pending: make(map[string]pendingAuth),
		client:  &http.Client{Timeout: 15 * time.Second},
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
	secrets := pendingAuth{codeVerifier: oauth2.GenerateVerifier()}
	if p.isOIDC() {
		if secrets.nonce, err = randomState(); err != nil {
			return "", "", fmt.Errorf("oauth: generating nonce: %w", err)
		}
	}
	p.remember(state, link, secrets)

	params := url.Values{
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {p.cfg.RedirectURL},
		"response_type":         {"code"},
		"state":                 {state},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(secrets.codeVerifier)},
		"code_challenge_method": {"S256"},
	}
	if p.isOIDC() {
		params.Set("scope", "openid email profile")
		params.Set("nonce", secrets.nonce)
	} else {
		params.Set("scope", "user:email")
	}
	return p.discovery.AuthorizationEndpoint + "?" +
		params.Encode(), state, nil
}

// isOIDC is true for providers that issue id_tokens (all but GitHub).
func (p *OAuthProvider) isOIDC() bool {
	return p.cfg.Provider != "github"
}

// remember stores a new pending state with its link intent and secrets.
func (p *OAuthProvider) remember(state string, link LinkIntent, secrets pendingAuth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpiredAndCapLocked()
	p.states[state] = time.Now().Add(10 * time.Minute)
	if p.pending == nil {
		p.pending = make(map[string]pendingAuth)
	}
	p.pending[state] = secrets
	if link.UserID > 0 {
		if p.links == nil {
			p.links = make(map[string]LinkIntent)
		}
		p.links[state] = link
	}
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
		return Identity{}, fmt.Errorf("%w: state cookie mismatch", ErrOAuthStateInvalid)
	}
	secrets, ok := p.consumeState(state)
	if !ok {
		return Identity{}, fmt.Errorf("%w: invalid or expired state", ErrOAuthStateInvalid)
	}
	if p.discovery == nil {
		return Identity{}, fmt.Errorf("oauth: discovery not performed")
	}
	token, err := p.exchangeCode(ctx, code, secrets.codeVerifier)
	if err != nil {
		return Identity{}, err
	}
	if !p.isOIDC() {
		return p.fetchGitHubIdentity(ctx, token.AccessToken)
	}
	return p.oidcIdentity(ctx, token, secrets.nonce)
}

// ValidateState checks and consumes a CSRF state token.
func (p *OAuthProvider) ValidateState(state string) bool {
	_, ok := p.consumeState(state)
	return ok
}

// consumeState removes a state and returns its secrets; ok is false for
// an unknown or expired state. A state is usable exactly once.
func (p *OAuthProvider) consumeState(state string) (pendingAuth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.states[state]
	if !ok {
		return pendingAuth{}, false
	}
	secrets := p.pending[state]
	p.dropStateLocked(state)
	return secrets, time.Now().Before(exp)
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
	delete(p.pending, state)
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

// tokenResponse is the part of a token endpoint reply pg_sage uses.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

// exchangeCode redeems an authorization code, proving possession of the
// PKCE verifier when one was sent.
func (p *OAuthProvider) exchangeCode(
	ctx context.Context, code, codeVerifier string,
) (tokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
	}
	if codeVerifier != "" {
		data.Set("code_verifier", codeVerifier)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.discovery.TokenEndpoint,
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("oauth: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("oauth: token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("oauth: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf(
			"oauth: token endpoint returned %d: %s",
			resp.StatusCode, string(body),
		)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("oauth: decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("oauth: empty access_token in response")
	}
	return tok, nil
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
