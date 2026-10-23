package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// maxMetadataBytes bounds an issuer's metadata document.
const maxMetadataBytes = 1 << 20

// issuerKeys verifies one issuer's tokens. Its key set is discovered on
// first use (never at startup, so an IdP outage cannot stop pg_sage), and a
// failed discovery is retried after retryAfter.
type issuerKeys struct {
	cfg    Issuer
	client *http.Client
	now    func() time.Time

	mu          sync.Mutex
	verifier    *oidc.IDTokenVerifier
	lastErr     error
	lastAttempt time.Time
}

func (k *issuerKeys) get(ctx context.Context, retryAfter time.Duration) (
	*oidc.IDTokenVerifier, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.verifier != nil {
		return k.verifier, nil
	}
	if k.lastErr != nil && k.now().Sub(k.lastAttempt) < retryAfter {
		return nil, k.lastErr
	}
	k.lastAttempt = k.now()
	jwks := k.cfg.JWKSURI
	if jwks == "" {
		var err error
		if jwks, err = k.discover(ctx); err != nil {
			k.lastErr = fmt.Errorf("%w: %s: %w", ErrIssuerUnavailable, k.cfg.Issuer, err)
			return nil, k.lastErr
		}
	}
	algs := k.cfg.SigningAlgs
	if len(algs) == 0 {
		algs = defaultAlgs
	}
	// The key set refetches on an unknown key id (rotation), with the
	// background context: a request's cancellation must not poison it.
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), k.client), jwks)
	k.verifier = oidc.NewVerifier(k.cfg.Issuer, keys, &oidc.Config{
		SkipClientIDCheck: true, SupportedSigningAlgs: algs, Now: k.now})
	k.lastErr = nil
	return k.verifier, nil
}

// discover reads the issuer's metadata (RFC 8414, then OpenID Connect
// Discovery) and returns its jwks_uri. The document must name this issuer
// exactly (mix-up defence).
func (k *issuerKeys) discover(ctx context.Context) (string, error) {
	var errs []error
	for _, u := range metadataURLs(k.cfg.Issuer) {
		jwks, err := k.fetchMetadata(ctx, u)
		if err == nil {
			return jwks, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

// metadataURLs are RFC 8414's well-known URI (inserted before the issuer's
// path) and OpenID Discovery's (appended to it).
func metadataURLs(issuer string) []string {
	trimmed := strings.TrimRight(issuer, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return []string{trimmed + "/.well-known/openid-configuration"}
	}
	rfc := u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server" + u.Path
	return []string{rfc, trimmed + "/.well-known/openid-configuration"}
}

func (k *issuerKeys) fetchMetadata(ctx context.Context, u string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("metadata request %s: %w", u, err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch metadata %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata %s: status %d", u, resp.StatusCode)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).
		Decode(&doc); err != nil {
		return "", fmt.Errorf("decode metadata %s: %w", u, err)
	}
	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(k.cfg.Issuer, "/") {
		return "", fmt.Errorf("metadata %s names issuer %q, not %q", u, doc.Issuer,
			k.cfg.Issuer)
	}
	if err := checkURL("jwks_uri", doc.JWKSURI); err != nil {
		return "", fmt.Errorf("metadata %s: %w", u, err)
	}
	return doc.JWKSURI, nil
}
