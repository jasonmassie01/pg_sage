package auth

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// D7: a link state carries the user it was issued for; a plain login state
// carries none, and every state is single use.

func linkStateProvider() *OAuthProvider {
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc", ClientID: "cid",
		RedirectURL: "https://sage.example/cb", IssuerURL: "https://idp.example.com"})
	p.discovery = &OIDCDiscovery{AuthorizationEndpoint: "https://idp.example.com/auth",
		TokenEndpoint: "https://idp.example.com/token"}
	return p
}

func TestAuthorizationURLForLink_StateCarriesUser(t *testing.T) {
	p := linkStateProvider()
	authURL, state, err := p.AuthorizationURLForLink(42, LinkViaSession)
	if err != nil {
		t.Fatalf("AuthorizationURLForLink: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil || parsed.Query().Get("state") != state {
		t.Fatalf("authorization URL does not carry the state: %s", authURL)
	}
	intent := p.LinkIntentForState(state)
	if intent.UserID != 42 || intent.Via != LinkViaSession {
		t.Fatalf("link intent = %+v, want user 42 via session", intent)
	}
}

func TestAuthorizationURLForLink_RejectsMissingUser(t *testing.T) {
	p := linkStateProvider()
	if _, _, err := p.AuthorizationURLForLink(0, LinkViaSession); err == nil {
		t.Fatal("link state issued without a user")
	}
}

func TestPlainLoginStateHasNoLinkIntent(t *testing.T) {
	p := linkStateProvider()
	_, state, err := p.AuthorizationURL()
	if err != nil {
		t.Fatal(err)
	}
	if intent := p.LinkIntentForState(state); intent.UserID != 0 {
		t.Fatalf("plain login state carries link intent %+v", intent)
	}
	if intent := p.LinkIntentForState("unknown-state"); intent.UserID != 0 {
		t.Fatalf("unknown state carries link intent %+v", intent)
	}
}

func TestLinkStateCannotBeReplayed(t *testing.T) {
	p := linkStateProvider()
	_, state, err := p.AuthorizationURLForLink(7, LinkViaGrant)
	if err != nil {
		t.Fatal(err)
	}
	if !p.ValidateState(state) {
		t.Fatal("fresh link state rejected")
	}
	if p.ValidateState(state) {
		t.Fatal("link state accepted twice")
	}
	if intent := p.LinkIntentForState(state); intent.UserID != 0 {
		t.Fatalf("consumed state still carries link intent %+v", intent)
	}
	if _, err := p.Exchange(context.Background(), "code", state, state); err == nil {
		t.Fatal("exchange accepted a consumed link state")
	}
}

func TestCleanStatesDropsExpiredLinkIntent(t *testing.T) {
	p := linkStateProvider()
	_, state, err := p.AuthorizationURLForLink(9, LinkViaSession)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.states[state] = p.states[state].Add(-11 * time.Minute)
	p.mu.Unlock()
	p.CleanStates()
	if intent := p.LinkIntentForState(state); intent.UserID != 0 {
		t.Fatalf("expired state still carries link intent %+v", intent)
	}
}
