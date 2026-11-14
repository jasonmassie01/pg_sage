package fakeidp_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/pg-sage/sidecar/internal/testsupport/fakeidp"
)

const clientID = "pg-sage-test-client"

// login is one client sign-in: the state, nonce and PKCE verifier it sends.
type login struct{ state, nonce, verifier string }

func newLogin(n int) login {
	return login{state: fmt.Sprintf("state-%02d", n), nonce: fmt.Sprintf("nonce-%02d", n),
		verifier: fmt.Sprintf("verifier-%02d-%s", n, strings.Repeat("v", 43))}
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeURL builds the URL a client sends the user to; an empty method
// leaves PKCE out.
func authorizeURL(idp *fakeidp.IdP, l login, method string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {idp.ClientID},
		"state": {l.state}, "nonce": {l.nonce}}
	if method != "" {
		q.Set("code_challenge", challenge(l.verifier))
		q.Set("code_challenge_method", method)
	}
	return idp.Issuer() + "/authorize?" + q.Encode()
}

// redeem posts a code to the token endpoint and returns status and body.
func redeem(idp *fakeidp.IdP, code, verifier string) (int, map[string]any, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	resp, err := http.PostForm(idp.Issuer()+"/token", form)
	if err != nil {
		return 0, nil, fmt.Errorf("post token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("decode token body: %w", err)
	}
	return resp.StatusCode, body, nil
}

// signIn plays a full S256 sign-in and returns the token response body.
func signIn(idp *fakeidp.IdP, l login) (map[string]any, error) {
	code, err := idp.Authorize(authorizeURL(idp, l, "S256"))
	if err != nil {
		return nil, err
	}
	status, body, err := redeem(idp, code, l.verifier)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("token status %d: %v", status, body)
	}
	return body, nil
}

func mustSignIn(t *testing.T, idp *fakeidp.IdP, l login) map[string]any {
	t.Helper()
	body, err := signIn(idp, l)
	if err != nil {
		t.Fatalf("sign in %s: %v", l.state, err)
	}
	return body
}

func getJSON(rawURL string, into any) (int, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return 0, fmt.Errorf("get %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return resp.StatusCode, nil
}

func discovery(t *testing.T, idp *fakeidp.IdP) (int, map[string]any) {
	t.Helper()
	var doc map[string]any
	status, err := getJSON(idp.Issuer()+"/.well-known/openid-configuration", &doc)
	if err != nil {
		t.Fatal(err)
	}
	return status, doc
}

// verifyIDToken checks raw against the JWKS the discovery document names,
// as a relying party does, and returns the header and the claims.
func verifyIDToken(idp *fakeidp.IdP, raw string) (jose.Header, map[string]any, error) {
	var doc map[string]any
	if _, err := getJSON(idp.Issuer()+"/.well-known/openid-configuration", &doc); err != nil {
		return jose.Header{}, nil, err
	}
	jwksURI, _ := doc["jwks_uri"].(string)
	var set jose.JSONWebKeySet
	if _, err := getJSON(jwksURI, &set); err != nil {
		return jose.Header{}, nil, err
	}
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return jose.Header{}, nil, fmt.Errorf("parse id_token: %w", err)
	}
	header := jws.Signatures[0].Header
	keys := set.Key(header.KeyID)
	if len(keys) != 1 {
		return header, nil, fmt.Errorf("JWKS has %d keys for kid %q", len(keys), header.KeyID)
	}
	payload, err := jws.Verify(keys[0])
	if err != nil {
		return header, nil, fmt.Errorf("verify id_token: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return header, nil, fmt.Errorf("decode claims: %w", err)
	}
	return header, claims, nil
}

func mustClaims(t *testing.T, idp *fakeidp.IdP, body map[string]any) map[string]any {
	t.Helper()
	raw, ok := body["id_token"].(string)
	if !ok {
		t.Fatalf("token body has no id_token: %v", body)
	}
	_, claims, err := verifyIDToken(idp, raw)
	if err != nil {
		t.Fatal(err)
	}
	return claims
}

func wantClaim(t *testing.T, claims map[string]any, name string, want any) {
	t.Helper()
	if got, ok := claims[name]; !ok || got != want {
		t.Errorf("claim %s = %v (present %v), want %v", name, got, ok, want)
	}
}
