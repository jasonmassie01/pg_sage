package mcpauth

import (
	"context"
	"encoding/base64"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	testResource = "https://sage.example.com/api/v1/mcp"
	testKeyID    = "k1"
)

// idp is an in-process authorization server: it publishes RFC 8414
// metadata and a JWKS, and signs access tokens.
type idp struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	mu       sync.Mutex
	issuer   string // the issuer the metadata claims; "" = srv.URL
	jwksHits atomic.Int64
	metaFail atomic.Bool
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &idp{t: t, key: key, kid: testKeyID}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", p.metadata)
	mux.HandleFunc("/jwks", p.jwks)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) URL() string { return p.srv.URL }

func (p *idp) metadata(w http.ResponseWriter, _ *http.Request) {
	if p.metaFail.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	p.mu.Lock()
	iss := p.issuer
	p.mu.Unlock()
	if iss == "" {
		iss = p.srv.URL
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss,
		"jwks_uri": p.srv.URL + "/jwks", "token_endpoint": p.srv.URL + "/token"})
}

func (p *idp) jwks(w http.ResponseWriter, _ *http.Request) {
	p.jwksHits.Add(1)
	p.mu.Lock()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey,
		KeyID: p.kid, Algorithm: "RS256", Use: "sig"}}}
	p.mu.Unlock()
	_ = json.NewEncoder(w).Encode(set)
}

// rotate replaces the signing key and its id.
func (p *idp) rotate(kid string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		p.t.Fatalf("rotate key: %v", err)
	}
	p.mu.Lock()
	p.key, p.kid = key, kid
	p.mu.Unlock()
}

// claims returns a valid claim set for subject sub with audience aud.
func (p *idp) claims(sub string, aud ...string) map[string]any {
	now := time.Now()
	return map[string]any{"iss": p.srv.URL, "sub": sub, "aud": aud,
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"scope": "pg_sage:read pg_sage:propose"}
}

// sign signs claims with the IdP's current key.
func (p *idp) sign(claims map[string]any) string {
	p.mu.Lock()
	key, kid := p.key, p.kid
	p.mu.Unlock()
	return signWith(p.t, jose.RS256, key, kid, claims)
}

func signWith(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string,
	claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("at+jwt").WithHeader("kid", kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	jws, err := signer.Sign(body)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

// mapResolver binds (issuer, subject) pairs to principal ids.
type mapResolver struct {
	mu    sync.Mutex
	binds map[[2]string]string
	err   error
	calls int
}

func (m *mapResolver) PrincipalForSubject(_ context.Context, iss, sub string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	id, ok := m.binds[[2]string{iss, sub}]
	if !ok {
		return "", ErrNoBinding
	}
	return id, nil
}

func newValidator(t *testing.T, p *idp, r Resolver, dbs ...string) *Validator {
	t.Helper()
	v, err := New(Config{Resource: testResource,
		Issuers: []Issuer{{Issuer: p.URL()}}, TaskClaim: "task_id"},
		r, func() []string { return dbs }, p.srv.Client())
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	return v
}

func boundResolver(p *idp, sub, principal string) *mapResolver {
	return &mapResolver{binds: map[[2]string]string{{p.URL(), sub}: principal}}
}

var errStorage = errors.New("control database unreachable")

func b64(v any) string {
	body, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(body)
}
