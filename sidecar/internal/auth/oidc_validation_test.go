package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/fakeidp"
)

// CG-03: OIDC login uses PKCE (S256), a per-login nonce, and a verified
// id_token (signature, issuer, audience, expiry, nonce).

func newFakeOIDC(t *testing.T) (*fakeidp.IdP, *OAuthProvider) {
	t.Helper()
	idp := fakeidp.New(t, "pg-sage-client")
	p := NewOAuthProvider(&config.OAuthConfig{
		Provider: "oidc", IssuerURL: idp.Issuer(), ClientID: "pg-sage-client",
		ClientSecret: "fixture-client-secret", RedirectURL: "https://sage.test/cb",
		GroupsClaim: "groups",
	})
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return idp, p
}

// login runs authorize -> provider sign-in -> callback exchange.
func login(t *testing.T, idp *fakeidp.IdP, p *OAuthProvider) (Identity, error) {
	t.Helper()
	authURL, state, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	code, err := idp.Authorize(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return p.Exchange(context.Background(), code, state, state)
}

func authParams(t *testing.T, p *OAuthProvider) url.Values {
	t.Helper()
	authURL, _, err := p.AuthorizationURL()
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestOIDCAuthorizationURL_CarriesPKCEAndNonce(t *testing.T) {
	_, p := newFakeOIDC(t)
	q := authParams(t, p)
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	challenge, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if err != nil || len(challenge) != sha256.Size {
		t.Fatalf("code_challenge %q is not a base64url SHA-256", q.Get("code_challenge"))
	}
	if len(q.Get("nonce")) < 32 {
		t.Fatalf("nonce %q is shorter than 128 bits of hex", q.Get("nonce"))
	}
	if q.Get("nonce") == q.Get("state") {
		t.Fatal("nonce equals state: they must be independent secrets")
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope %q lacks openid", q.Get("scope"))
	}
}

func TestOIDCAuthorizationURL_FreshSecretsPerLogin(t *testing.T) {
	_, p := newFakeOIDC(t)
	a, b := authParams(t, p), authParams(t, p)
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if a.Get(k) == b.Get(k) {
			t.Fatalf("%s repeated across two logins: %q", k, a.Get(k))
		}
	}
}

func TestGitHubAuthorizationURL_PKCEWithoutNonce(t *testing.T) {
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "github", ClientID: "gh"})
	if err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := authParams(t, p)
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("github authorize lacks PKCE: %v", q)
	}
	if q.Has("nonce") {
		t.Fatal("github (OAuth 2, no id_token) must not send a nonce")
	}
}

func TestOIDCExchange_ValidTokenYieldsIdentity(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.SetClaims(map[string]any{"sub": "sub-42", "email": "dba@corp.invalid",
		"groups": []string{"dba", "sre"}})
	id, err := login(t, idp, p)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Issuer != idp.Issuer() || id.Subject != "sub-42" ||
		id.Email != "dba@corp.invalid" || !id.EmailVerified {
		t.Fatalf("identity = %+v", id)
	}
	if fmt.Sprint(id.Groups) != "[dba sre]" {
		t.Fatalf("groups = %v, want [dba sre]", id.Groups)
	}
	calls := idp.TokenRequests()
	if len(calls) != 1 || calls[0].Get("code_verifier") == "" {
		t.Fatalf("token request carried no code_verifier: %v", calls)
	}
}

func TestOIDCExchange_ForgedNonceRejected(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.SetClaims(map[string]any{"nonce": "attacker-chosen-nonce"})
	id, err := login(t, idp, p)
	if !errors.Is(err, ErrOIDCNonceMismatch) {
		t.Fatalf("forged nonce: err = %v, want ErrOIDCNonceMismatch", err)
	}
	if id.Subject != "" || id.Email != "" {
		t.Fatalf("forged nonce returned an identity: %+v", id)
	}
}

func TestOIDCExchange_MissingNonceRejected(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.SetClaims(map[string]any{"nonce": nil})
	if _, err := login(t, idp, p); !errors.Is(err, ErrOIDCNonceMismatch) {
		t.Fatalf("missing nonce: err = %v, want ErrOIDCNonceMismatch", err)
	}
}

func TestOIDCExchange_RejectsInvalidIDTokens(t *testing.T) {
	cases := map[string]func(*fakeidp.IdP){
		"wrong audience": func(i *fakeidp.IdP) {
			i.SetClaims(map[string]any{"aud": "someone-else"})
		},
		"wrong issuer": func(i *fakeidp.IdP) {
			i.SetClaims(map[string]any{"iss": "https://evil.invalid"})
		},
		"expired": func(i *fakeidp.IdP) {
			i.SetClaims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
		},
		"no expiry":       func(i *fakeidp.IdP) { i.SetClaims(map[string]any{"exp": nil}) },
		"foreign signer":  func(i *fakeidp.IdP) { i.ForgeSignature(true) },
		"empty subject":   func(i *fakeidp.IdP) { i.SetClaims(map[string]any{"sub": ""}) },
		"audience absent": func(i *fakeidp.IdP) { i.SetClaims(map[string]any{"aud": nil}) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			idp, p := newFakeOIDC(t)
			mutate(idp)
			id, err := login(t, idp, p)
			if !errors.Is(err, ErrOIDCIDTokenInvalid) {
				t.Fatalf("err = %v, want ErrOIDCIDTokenInvalid", err)
			}
			if id.Subject != "" {
				t.Fatalf("invalid token returned identity %+v", id)
			}
		})
	}
}

func TestOIDCExchange_UnsignedTokenRejected(t *testing.T) {
	idp, p := newFakeOIDC(t)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"iss":%q,"aud":"pg-sage-client","sub":"x","exp":%d}`,
		idp.Issuer(), time.Now().Add(time.Hour).Unix())))
	_, err := p.verifyIDToken(context.Background(), header+"."+body+".", "n")
	if !errors.Is(err, ErrOIDCIDTokenInvalid) {
		t.Fatalf("alg=none token: err = %v, want ErrOIDCIDTokenInvalid", err)
	}
}

func TestOIDCExchange_MissingIDTokenRejected(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.OmitIDToken(true)
	idp.SetUserinfo(map[string]any{"sub": "s", "email": "a@b.invalid", "email_verified": true})
	if _, err := login(t, idp, p); !errors.Is(err, ErrOIDCIDTokenMissing) {
		t.Fatalf("no id_token: err = %v, want ErrOIDCIDTokenMissing", err)
	}
}

func TestOIDCExchange_WrongPKCEVerifierFailsAtProvider(t *testing.T) {
	idp, p := newFakeOIDC(t)
	authURL, state, _ := p.AuthorizationURL()
	u, _ := url.Parse(authURL)
	q := u.Query()
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	u.RawQuery = q.Encode()
	code, _ := idp.Authorize(u.String())
	_, err := p.Exchange(context.Background(), code, state, state)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("mismatched PKCE: err = %v, want token endpoint 400", err)
	}
}

func TestOIDCExchange_StateIsSingleUse(t *testing.T) {
	idp, p := newFakeOIDC(t)
	authURL, state, _ := p.AuthorizationURL()
	code, _ := idp.Authorize(authURL)
	if _, err := p.Exchange(context.Background(), code, state, state); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	_, err := p.Exchange(context.Background(), code, state, state)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("replayed state: err = %v, want a state error", err)
	}
}

func TestOIDCExchange_EmailFromUserinfoWhenAbsentFromIDToken(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.SetClaims(map[string]any{"sub": "sub-u", "email": nil, "email_verified": nil})
	idp.SetUserinfo(map[string]any{"sub": "sub-u", "email": "u@corp.invalid",
		"email_verified": true, "groups": []string{"from-userinfo"}})
	id, err := login(t, idp, p)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "sub-u" || id.Email != "u@corp.invalid" {
		t.Fatalf("identity = %+v", id)
	}
	if fmt.Sprint(id.Groups) != "[from-userinfo]" {
		t.Fatalf("groups = %v, want userinfo groups", id.Groups)
	}
}

func TestOIDCExchange_UserinfoSubjectMustMatchIDToken(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.SetClaims(map[string]any{"sub": "sub-a", "email": nil, "email_verified": nil})
	idp.SetUserinfo(map[string]any{"sub": "sub-b", "email": "b@corp.invalid",
		"email_verified": true})
	if _, err := login(t, idp, p); !errors.Is(err, ErrOIDCSubjectMismatch) {
		t.Fatalf("userinfo sub mismatch: err = %v, want ErrOIDCSubjectMismatch", err)
	}
}

func TestOIDCExchange_UnverifiedEmailRejected(t *testing.T) {
	idp, p := newFakeOIDC(t)
	idp.SetClaims(map[string]any{"email_verified": false})
	if _, err := login(t, idp, p); !errors.Is(err, ErrOAuthEmailUnverified) {
		t.Fatalf("unverified: err = %v, want ErrOAuthEmailUnverified", err)
	}
}

func TestOIDCExchange_GroupsClaimShapes(t *testing.T) {
	cases := map[string]struct {
		claim any
		want  string
	}{
		"array":         {[]string{"a", "b"}, "[a b]"},
		"single string": {"solo", "[solo]"},
		"mixed array":   {[]any{"a", 7, "b"}, "[a b]"},
		"number":        {42, "[]"},
		"absent":        {nil, "[]"},
		"empty array":   {[]string{}, "[]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			idp, p := newFakeOIDC(t)
			idp.SetClaims(map[string]any{"groups": tc.claim})
			id, err := login(t, idp, p)
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if fmt.Sprint(id.Groups) != tc.want {
				t.Fatalf("groups = %v, want %s", id.Groups, tc.want)
			}
		})
	}
}

func TestOIDCExchange_CustomGroupsClaim(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc", IssuerURL: idp.Issuer(),
		ClientID: "cid", RedirectURL: "https://sage.test/cb", GroupsClaim: "roles"})
	if err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	idp.SetClaims(map[string]any{"roles": []string{"pg-admins"}, "groups": []string{"x"}})
	id, err := login(t, idp, p)
	if err != nil || fmt.Sprint(id.Groups) != "[pg-admins]" {
		t.Fatalf("custom claim: groups = %v, err = %v", id.Groups, err)
	}
}

func TestOIDCDiscover_RejectsIssuerMismatch(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.SetDiscovery(map[string]any{"issuer": "https://other-issuer.invalid"})
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc",
		IssuerURL: idp.Issuer(), ClientID: "cid"})
	err := p.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("issuer mismatch: err = %v, want issuer error", err)
	}
}

func TestOIDCDiscover_RequiresJWKS(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.SetDiscovery(map[string]any{"jwks_uri": nil})
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc",
		IssuerURL: idp.Issuer(), ClientID: "cid"})
	err := p.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "jwks_uri") {
		t.Fatalf("missing jwks_uri: err = %v", err)
	}
}

func TestOIDCDiscover_TrailingSlashIssuerMatches(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc",
		IssuerURL: idp.Issuer() + "/", ClientID: "cid", RedirectURL: "https://s/cb"})
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover with trailing slash: %v", err)
	}
	if _, err := login(t, idp, p); err != nil {
		t.Fatalf("login with trailing-slash issuer config: %v", err)
	}
}

func TestOIDCExchange_ConcurrentLoginsKeepTheirOwnSecrets(t *testing.T) {
	idp, p := newFakeOIDC(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			authURL, state, err := p.AuthorizationURL()
			if err != nil {
				errs <- err
				return
			}
			code, _ := idp.Authorize(authURL)
			if _, err := p.Exchange(context.Background(), code, state, state); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent login: %v", err)
	}
}

func TestOIDCPendingSecretsDroppedWithState(t *testing.T) {
	_, p := newFakeOIDC(t)
	_, state, _ := p.AuthorizationURL()
	p.mu.Lock()
	p.states[state] = time.Now().Add(-time.Minute)
	p.mu.Unlock()
	p.CleanStates()
	p.mu.RLock()
	_, kept := p.pending[state]
	p.mu.RUnlock()
	if kept {
		t.Fatal("expired state's nonce and verifier survived cleanup")
	}
}

func TestOIDCExchange_DiscoveryNotPerformed(t *testing.T) {
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc", ClientID: "c"})
	_, err := p.Exchange(context.Background(), "c", "s", "s")
	if err == nil {
		t.Fatal("Exchange without discovery succeeded")
	}
}
