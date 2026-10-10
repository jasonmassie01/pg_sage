package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const principalA = "agp_aaaaaaaaaaaaaaaaaaaa"

// A token whose audience is database A's resource is accepted at A and
// binds the principal to A only.
func TestValidTokenBindsPrincipalAndDatabase(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders", "billing")
	raw := p.sign(p.claims("agent-1", ResourceFor(testResource, "orders")))
	id, err := v.Validate(context.Background(), raw, ResourceFor(testResource, "orders"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if id.PrincipalID != principalA || id.Issuer != p.URL() || id.Subject != "agent-1" {
		t.Fatalf("identity = %+v", id)
	}
	if len(id.Databases) != 1 || id.Databases[0] != "orders" {
		t.Fatalf("databases = %v, want [orders]", id.Databases)
	}
	if strings.Join(id.Scopes, ",") != "read,propose" {
		t.Fatalf("scopes = %v, want read and propose", id.Scopes)
	}
	if id.Expiry.Before(time.Now()) {
		t.Fatalf("expiry %v not carried", id.Expiry)
	}
}

// G1-03: a token for database A is refused on B, and a foreign audience
// is refused everywhere.
func TestAudienceIsThisSageAndThisDatabase(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders", "billing")
	forA := p.sign(p.claims("agent-1", ResourceFor(testResource, "orders")))
	_, err := v.Validate(context.Background(), forA, ResourceFor(testResource, "billing"))
	if !errors.Is(err, ErrAudience) {
		t.Fatalf("token for orders at billing: err = %v, want ErrAudience", err)
	}
	foreign := p.sign(p.claims("agent-1", "https://other-api.example.com"))
	for _, res := range []string{testResource, ResourceFor(testResource, "orders")} {
		if _, err := v.Validate(context.Background(), foreign, res); !errors.Is(err, ErrAudience) {
			t.Fatalf("foreign aud at %s: err = %v, want ErrAudience", res, err)
		}
	}
	other := p.sign(p.claims("agent-1", "https://other-sage.example.com/api/v1/mcp"))
	if _, err := v.Validate(context.Background(), other, testResource); !errors.Is(err, ErrAudience) {
		t.Fatalf("another pg_sage's audience: err = %v, want ErrAudience", err)
	}
}

// At the server-wide resource the token's database audiences are an upper
// bound; with none, the principal reaches no database.
func TestServerResourceDatabasesComeFromAudience(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders", "billing")
	ctx := context.Background()
	raw := p.sign(p.claims("agent-1", testResource, ResourceFor(testResource, "billing"),
		ResourceFor(testResource, "unknown-db")))
	id, err := v.Validate(ctx, raw, testResource)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(id.Databases) != 1 || id.Databases[0] != "billing" {
		t.Fatalf("databases = %v, want [billing] (unknown databases dropped)", id.Databases)
	}
	bare := p.sign(p.claims("agent-1", testResource))
	id, err = v.Validate(ctx, bare, testResource)
	if err != nil {
		t.Fatalf("validate bare: %v", err)
	}
	if id.Databases == nil || len(id.Databases) != 0 {
		t.Fatalf("databases = %#v, want an empty non-nil list (none, never all)",
			id.Databases)
	}
}

// Expired, not-yet-valid, wrongly signed, unsigned and malformed tokens are
// invalid; none reaches the resolver.
func TestInvalidTokensAreRefused(t *testing.T) {
	p := newIDP(t)
	r := boundResolver(p, "agent-1", principalA)
	v := newValidator(t, p, r, "orders")
	aud := ResourceFor(testResource, "orders")
	expired := p.claims("agent-1", aud)
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	future := p.claims("agent-1", aud)
	future["nbf"] = time.Now().Add(time.Hour).Unix()
	noExp := p.claims("agent-1", aud)
	delete(noExp, "exp")
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	cases := map[string]string{
		"expired":       p.sign(expired),
		"not yet valid": p.sign(future),
		"no expiry":     p.sign(noExp),
		"wrong key": signWith(t, jose.RS256, otherKey, testKeyID,
			p.claims("agent-1", aud)),
		"hmac": signWith(t, jose.HS256, []byte(strings.Repeat("k", 32)), testKeyID,
			p.claims("agent-1", aud)),
		"empty":          "",
		"garbage":        "not-a-token",
		"three segments": "a.b.c",
		"oversized":      strings.Repeat("a", MaxTokenBytes+1),
		"unsigned":       unsignedToken(p.claims("agent-1", aud)),
	}
	for name, raw := range cases {
		_, err := v.Validate(context.Background(), raw, aud)
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
	if r.calls != 0 {
		t.Fatalf("resolver consulted %d times for invalid tokens", r.calls)
	}
}

// A token from an issuer outside the allowlist is refused before any key
// is fetched for it.
func TestUnknownIssuerIsRefused(t *testing.T) {
	p, stranger := newIDP(t), newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders")
	raw := stranger.sign(stranger.claims("agent-1", ResourceFor(testResource, "orders")))
	_, err := v.Validate(context.Background(), raw, ResourceFor(testResource, "orders"))
	if !errors.Is(err, ErrUnknownIssuer) {
		t.Fatalf("err = %v, want ErrUnknownIssuer", err)
	}
	if stranger.jwksHits.Load() != 0 {
		t.Fatalf("the stranger's keys were fetched")
	}
}

// A valid token for an unbound identity binds nothing; a resolver failure
// is distinguishable from a bad token.
func TestBindingOutcomes(t *testing.T) {
	p := newIDP(t)
	aud := ResourceFor(testResource, "orders")
	v := newValidator(t, p, boundResolver(p, "someone-else", principalA), "orders")
	_, err := v.Validate(context.Background(), p.sign(p.claims("agent-1", aud)), aud)
	if !errors.Is(err, ErrNoBinding) {
		t.Fatalf("unbound: err = %v, want ErrNoBinding", err)
	}
	broken := &mapResolver{err: errStorage}
	v = newValidator(t, p, broken, "orders")
	_, err = v.Validate(context.Background(), p.sign(p.claims("agent-1", aud)), aud)
	if !errors.Is(err, ErrResolver) || !errors.Is(err, errStorage) ||
		errors.Is(err, ErrInvalidToken) {
		t.Fatalf("resolver failure: err = %v, want ErrResolver wrapping the cause", err)
	}
	if _, err := New(Config{Resource: testResource, Issuers: []Issuer{{Issuer: p.URL()}}},
		nil, nil, nil); err == nil {
		t.Fatalf("a validator without a resolver was built")
	}
}

// Scopes: read is required, approve is never granted, and the scp array
// form some issuers use is read too.
func TestScopes(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders")
	aud := ResourceFor(testResource, "orders")
	ctx := context.Background()
	none := p.claims("agent-1", aud)
	delete(none, "scope")
	if _, err := v.Validate(ctx, p.sign(none), aud); !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("no scope: err = %v, want ErrInsufficientScope", err)
	}
	approve := p.claims("agent-1", aud)
	approve["scope"] = "pg_sage:read pg_sage:approve openid"
	id, err := v.Validate(ctx, p.sign(approve), aud)
	if err != nil || strings.Join(id.Scopes, ",") != "read" {
		t.Fatalf("approve requested: scopes = %v err = %v, want read only", id.Scopes, err)
	}
	scp := p.claims("agent-1", aud)
	delete(scp, "scope")
	scp["scp"] = []string{"pg_sage:read", "pg_sage:propose"}
	id, err = v.Validate(ctx, p.sign(scp), aud)
	if err != nil || len(id.Scopes) != 2 {
		t.Fatalf("scp array: scopes = %v err = %v", id.Scopes, err)
	}
}

// RFC 8693 delegation: the actor (act.sub) is the agent the principal binds
// to; the token's subject is whom it acts for. A task claim fills TaskID.
func TestDelegationAndTask(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders")
	aud := ResourceFor(testResource, "orders")
	c := p.claims("alice@example.com", aud)
	c["act"] = map[string]any{"sub": "agent-1",
		"act": map[string]any{"sub": "orchestrator"}}
	c["task_id"] = "TASK-42"
	id, err := v.Validate(context.Background(), p.sign(c), aud)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if id.PrincipalID != principalA || id.Subject != "agent-1" ||
		id.OnBehalfOf != "alice@example.com" || id.TaskID != "TASK-42" {
		t.Fatalf("identity = %+v", id)
	}
	if strings.Join(id.DelegationChain, ">") != "orchestrator" {
		t.Fatalf("delegation chain = %v, want [orchestrator]", id.DelegationChain)
	}
	bad := p.claims("agent-1", aud)
	bad["task_id"] = strings.Repeat("t", 300)
	if _, err := v.Validate(context.Background(), p.sign(bad), aud); !errors.Is(err,
		ErrInvalidToken) {
		t.Fatalf("oversized task claim: err = %v, want ErrInvalidToken", err)
	}
	notString := p.claims("agent-1", aud)
	notString["task_id"] = 42
	if _, err := v.Validate(context.Background(), p.sign(notString), aud); !errors.Is(err,
		ErrInvalidToken) {
		t.Fatalf("numeric task claim: err = %v, want ErrInvalidToken", err)
	}
}

// Keys are discovered from the issuer's metadata, a failed discovery is
// retried on a later request, and a rotated key is fetched on demand.
func TestDiscoveryRetryAndRotation(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders")
	aud := ResourceFor(testResource, "orders")
	ctx := context.Background()
	p.metaFail.Store(true)
	if _, err := v.Validate(ctx, p.sign(p.claims("agent-1", aud)), aud); !errors.Is(err,
		ErrIssuerUnavailable) {
		t.Fatalf("metadata down: err = %v, want ErrIssuerUnavailable", err)
	}
	p.metaFail.Store(false)
	v.retryAfter = 0
	if _, err := v.Validate(ctx, p.sign(p.claims("agent-1", aud)), aud); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	p.rotate("k2")
	if _, err := v.Validate(ctx, p.sign(p.claims("agent-1", aud)), aud); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

// Metadata naming another issuer is refused (mix-up defence).
func TestDiscoveryIssuerMismatch(t *testing.T) {
	p := newIDP(t)
	p.issuer = "https://evil.example.com"
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders")
	aud := ResourceFor(testResource, "orders")
	_, err := v.Validate(context.Background(), p.sign(p.claims("agent-1", aud)), aud)
	if !errors.Is(err, ErrIssuerUnavailable) || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("mismatched metadata: err = %v", err)
	}
}

// Concurrent validations share the key cache safely.
func TestConcurrentValidation(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, boundResolver(p, "agent-1", principalA), "orders")
	aud := ResourceFor(testResource, "orders")
	raw := p.sign(p.claims("agent-1", aud))
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Validate(context.Background(), raw, aud)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent validate: %v", err)
		}
	}
}

// Configuration is checked up front.
func TestConfigValidation(t *testing.T) {
	r := &mapResolver{}
	bad := []Config{
		{},
		{Resource: "ftp://sage.example.com/mcp", Issuers: []Issuer{{Issuer: "https://idp"}}},
		{Resource: "http://sage.example.com/mcp", Issuers: []Issuer{{Issuer: "https://idp"}}},
		{Resource: testResource + "#frag", Issuers: []Issuer{{Issuer: "https://idp"}}},
		{Resource: testResource},
		{Resource: testResource, Issuers: []Issuer{{Issuer: "http://idp.example.com"}}},
		{Resource: testResource,
			Issuers: []Issuer{{Issuer: "https://idp"}, {Issuer: "https://idp"}}},
		{Resource: testResource, Issuers: []Issuer{{Issuer: "https://idp",
			SigningAlgs: []string{"none"}}}},
		{Resource: testResource, Issuers: []Issuer{{Issuer: "https://idp",
			SigningAlgs: []string{"HS256"}}}},
	}
	for i, c := range bad {
		if _, err := New(c, r, nil, nil); err == nil {
			t.Fatalf("bad config %d accepted: %+v", i, c)
		}
	}
	ok := Config{Resource: "http://localhost:8080/api/v1/mcp",
		Issuers: []Issuer{{Issuer: "http://127.0.0.1:9000"}}}
	if _, err := New(ok, r, nil, nil); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
}

func unsignedToken(claims map[string]any) string {
	return "eyJhbGciOiJub25lIn0." + b64(claims) + "."
}
