// Package fakeidp is an in-process OpenID Connect provider for tests. It
// serves discovery, a JWKS, a token endpoint that enforces PKCE and signs
// RS256 id_tokens, and a userinfo endpoint. Tests shape the next token's
// claims to exercise every id_token validation pg_sage performs.
package fakeidp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const keyID = "fakeidp-k1"

var (
	keyOnce    sync.Once
	signingKey *rsa.PrivateKey
	foreignKey *rsa.PrivateKey
	keyErr     error
)

// keys returns the process-wide signing key and an unrelated key used to
// forge signatures. RSA generation is slow, so both are made once.
func keys() (*rsa.PrivateKey, *rsa.PrivateKey, error) {
	keyOnce.Do(func() {
		signingKey, keyErr = rsa.GenerateKey(rand.Reader, 2048)
		if keyErr == nil {
			foreignKey, keyErr = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	return signingKey, foreignKey, keyErr
}

// IdP is one fake provider. All setters are safe for concurrent use.
type IdP struct {
	Server   *httptest.Server
	ClientID string

	mu         sync.Mutex
	pending    map[string]authRequest // code -> authorize request
	claims     map[string]any         // overrides merged into the next token
	userinfo   map[string]any
	discovery  map[string]any // overrides merged into discovery
	omitID     bool
	forgeSig   bool
	tokenCalls []url.Values
}

type authRequest struct {
	nonce     string
	challenge string
	method    string
}

// New starts a fake IdP for clientID; it stops when the test ends.
func New(t testing.TB, clientID string) *IdP {
	t.Helper()
	if _, _, err := keys(); err != nil {
		t.Fatalf("fakeidp: generating keys: %v", err)
	}
	idp := &IdP{ClientID: clientID, pending: map[string]authRequest{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.serveDiscovery)
	mux.HandleFunc("/jwks", idp.serveJWKS)
	mux.HandleFunc("/token", idp.serveToken)
	mux.HandleFunc("/userinfo", idp.serveUserinfo)
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Server.Close)
	return idp
}

// Issuer is the provider's issuer URL (the server root).
func (i *IdP) Issuer() string { return i.Server.URL }

// Authorize records the nonce and PKCE challenge of an authorization URL
// built by the client and returns the code the provider would redirect
// back with. It plays the user's sign-in at the provider.
func (i *IdP) Authorize(authURL string) (string, error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return "", fmt.Errorf("fakeidp: parsing authorize url: %w", err)
	}
	q := u.Query()
	code := "code-" + q.Get("state")
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pending[code] = authRequest{
		nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		method: q.Get("code_challenge_method"),
	}
	return code, nil
}

// SetClaims replaces the claim overrides applied to every issued id_token.
// A nil value removes that claim from the token.
func (i *IdP) SetClaims(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.claims = claims
}

// SetUserinfo sets the userinfo response body.
func (i *IdP) SetUserinfo(info map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.userinfo = info
}

// SetDiscovery merges overrides into the discovery document.
func (i *IdP) SetDiscovery(overrides map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.discovery = overrides
}

// OmitIDToken makes the token endpoint return only an access token.
func (i *IdP) OmitIDToken(omit bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.omitID = omit
}

// ForgeSignature signs id_tokens with a key absent from the JWKS.
func (i *IdP) ForgeSignature(forge bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.forgeSig = forge
}

// TokenRequests returns the form bodies the token endpoint received.
func (i *IdP) TokenRequests() []url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]url.Values(nil), i.tokenCalls...)
}

func (i *IdP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                i.Issuer(),
		"authorization_endpoint":                i.Issuer() + "/authorize",
		"token_endpoint":                        i.Issuer() + "/token",
		"userinfo_endpoint":                     i.Issuer() + "/userinfo",
		"jwks_uri":                              i.Issuer() + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	i.mu.Lock()
	for k, v := range i.discovery {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	i.mu.Unlock()
	writeJSON(w, http.StatusOK, doc)
}

func (i *IdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	key, _, _ := keys()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: keyID, Algorithm: "RS256", Use: "sig",
	}}}
	writeJSON(w, http.StatusOK, set)
}

func (i *IdP) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	i.mu.Lock()
	i.tokenCalls = append(i.tokenCalls, r.PostForm)
	req, ok := i.pending[r.PostForm.Get("code")]
	delete(i.pending, r.PostForm.Get("code"))
	i.mu.Unlock()
	if !ok || !pkceMatches(req, r.PostForm.Get("code_verifier")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	body := map[string]string{"access_token": "fake-access", "token_type": "Bearer"}
	if !i.omitIDTokenSet() {
		token, err := i.signIDToken(req.nonce)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		body["id_token"] = token
	}
	writeJSON(w, http.StatusOK, body)
}

func (i *IdP) omitIDTokenSet() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.omitID
}

// pkceMatches enforces RFC 7636 S256: a request that sent a challenge must
// redeem with the matching verifier.
func pkceMatches(req authRequest, verifier string) bool {
	if req.challenge == "" {
		return verifier == ""
	}
	if req.method != "S256" || verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == req.challenge
}

func (i *IdP) signIDToken(nonce string) (string, error) {
	key, foreign, _ := keys()
	now := time.Now()
	claims := map[string]any{
		"iss": i.Issuer(), "aud": i.ClientID, "sub": "fake-subject",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": nonce,
		"email": "fake-user@idp.invalid", "email_verified": true,
	}
	i.mu.Lock()
	for k, v := range i.claims {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	signWith := key
	if i.forgeSig {
		signWith = foreign
	}
	i.mu.Unlock()
	return SignClaims(signWith, claims)
}

// SignClaims returns a compact RS256 JWS over claims with the fake key id.
func SignClaims(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256,
			Key: jose.JSONWebKey{Key: key, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", fmt.Errorf("fakeidp: signer: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("fakeidp: claims: %w", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("fakeidp: signing: %w", err)
	}
	return obj.CompactSerialize()
}

func (i *IdP) serveUserinfo(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	info := i.userinfo
	i.mu.Unlock()
	if info == nil {
		info = map[string]any{"sub": "fake-subject"}
	}
	writeJSON(w, http.StatusOK, info)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
