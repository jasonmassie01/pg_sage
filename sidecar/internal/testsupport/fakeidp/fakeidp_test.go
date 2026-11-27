package fakeidp_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/pg-sage/sidecar/internal/testsupport/fakeidp"
)

func TestDiscoveryNamesEndpointsUnderTheIssuer(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	status, doc := discovery(t, idp)
	if status != http.StatusOK {
		t.Fatalf("discovery status = %d", status)
	}
	want := map[string]string{"issuer": idp.Issuer(),
		"authorization_endpoint": idp.Issuer() + "/authorize",
		"token_endpoint":         idp.Issuer() + "/token",
		"userinfo_endpoint":      idp.Issuer() + "/userinfo",
		"jwks_uri":               idp.Issuer() + "/jwks"}
	for key, value := range want {
		if doc[key] != value {
			t.Errorf("%s = %v, want %s", key, doc[key], value)
		}
	}
	algs, _ := doc["id_token_signing_alg_values_supported"].([]any)
	if len(algs) != 1 || algs[0] != "RS256" {
		t.Errorf("signing algs = %v, want [RS256]", algs)
	}
	if idp.Issuer() != idp.Server.URL || idp.ClientID != clientID {
		t.Errorf("issuer %q client %q", idp.Issuer(), idp.ClientID)
	}
}

func TestIDTokenVerifiesAgainstTheServedJWKS(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	l := newLogin(1)
	body := mustSignIn(t, idp, l)
	if body["access_token"] != "fake-access" || body["token_type"] != "Bearer" {
		t.Errorf("token body = %v", body)
	}
	header, claims, err := verifyIDToken(idp, body["id_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if header.KeyID != "fakeidp-k1" || header.Algorithm != "RS256" {
		t.Errorf("header kid %q alg %q", header.KeyID, header.Algorithm)
	}
	wantClaim(t, claims, "iss", idp.Issuer())
	wantClaim(t, claims, "aud", clientID)
	wantClaim(t, claims, "sub", "fake-subject")
	wantClaim(t, claims, "nonce", l.nonce)
	wantClaim(t, claims, "email", "fake-user@idp.invalid")
	wantClaim(t, claims, "email_verified", true)
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	now := float64(time.Now().Unix())
	if iat < now-60 || iat > now+1 || exp-iat != 300 {
		t.Errorf("iat %v exp %v, want iat about now and a five-minute life", iat, exp)
	}
}

// State transition: claims changed between two logins reach the next token,
// and SetClaims replaces the overrides rather than merging them.
func TestSetClaimsTakesEffectOnTheNextLogin(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	first := mustClaims(t, idp, mustSignIn(t, idp, newLogin(1)))
	wantClaim(t, first, "sub", "fake-subject")

	idp.SetClaims(map[string]any{"sub": "user-2", "email": nil, "role": "dba"})
	second := mustClaims(t, idp, mustSignIn(t, idp, newLogin(2)))
	wantClaim(t, second, "sub", "user-2")
	wantClaim(t, second, "role", "dba")
	wantClaim(t, second, "iss", idp.Issuer())
	if _, ok := second["email"]; ok {
		t.Errorf("a nil override left email in the token: %v", second["email"])
	}

	idp.SetClaims(map[string]any{"aud": "other-client", "iss": "https://evil.invalid"})
	third := mustClaims(t, idp, mustSignIn(t, idp, newLogin(3)))
	wantClaim(t, third, "aud", "other-client")
	wantClaim(t, third, "iss", "https://evil.invalid")
	wantClaim(t, third, "sub", "fake-subject")
	wantClaim(t, third, "email", "fake-user@idp.invalid")
	if _, ok := third["role"]; ok {
		t.Error("the previous override set leaked into the next token")
	}
}

// Nil/empty: SetClaims(nil) and an empty map remove every override.
func TestSetClaimsNilRestoresTheDefaults(t *testing.T) {
	for _, reset := range []map[string]any{nil, {}} {
		idp := fakeidp.New(t, clientID)
		idp.SetClaims(map[string]any{"iss": "x", "aud": "y", "sub": "z"})
		over := mustClaims(t, idp, mustSignIn(t, idp, newLogin(1)))
		wantClaim(t, over, "iss", "x")
		wantClaim(t, over, "aud", "y")
		wantClaim(t, over, "sub", "z")
		idp.SetClaims(reset)
		back := mustClaims(t, idp, mustSignIn(t, idp, newLogin(2)))
		wantClaim(t, back, "iss", idp.Issuer())
		wantClaim(t, back, "aud", clientID)
		wantClaim(t, back, "sub", "fake-subject")
	}
}

func TestOmitIDTokenTogglesTheIDToken(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	idp.OmitIDToken(true)
	body := mustSignIn(t, idp, newLogin(1))
	if _, ok := body["id_token"]; ok || body["access_token"] != "fake-access" {
		t.Errorf("omitted: body = %v, want only the access token", body)
	}
	idp.OmitIDToken(false)
	mustClaims(t, idp, mustSignIn(t, idp, newLogin(2)))
}

func TestSetDiscoveryMergesAndRemovesKeys(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	idp.SetDiscovery(map[string]any{"issuer": "https://other.invalid",
		"userinfo_endpoint": nil, "end_session_endpoint": "https://other.invalid/logout"})
	_, doc := discovery(t, idp)
	if doc["issuer"] != "https://other.invalid" ||
		doc["end_session_endpoint"] != "https://other.invalid/logout" {
		t.Errorf("overrides not applied: %v", doc)
	}
	if _, ok := doc["userinfo_endpoint"]; ok {
		t.Error("a nil override left userinfo_endpoint in the document")
	}
	if doc["token_endpoint"] != idp.Issuer()+"/token" {
		t.Errorf("an untouched key changed: token_endpoint = %v", doc["token_endpoint"])
	}
	idp.SetDiscovery(nil)
	if _, doc = discovery(t, idp); doc["issuer"] != idp.Issuer() ||
		doc["userinfo_endpoint"] != idp.Issuer()+"/userinfo" {
		t.Errorf("SetDiscovery(nil) kept overrides: %v", doc)
	}
}

func TestForgeSignatureFailsVerification(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	idp.ForgeSignature(true)
	body := mustSignIn(t, idp, newLogin(1))
	header, _, err := verifyIDToken(idp, body["id_token"].(string))
	if err == nil || !strings.Contains(err.Error(), "verify id_token") {
		t.Fatalf("forged token: err = %v, want a signature failure", err)
	}
	if header.KeyID != "fakeidp-k1" {
		t.Errorf("forged kid = %q, want the served kid so only the signature is wrong",
			header.KeyID)
	}
	idp.ForgeSignature(false)
	mustClaims(t, idp, mustSignIn(t, idp, newLogin(2)))
}

func TestFailDiscoveryFailsTheNextRequestsThenServes(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	idp.FailDiscovery(2, http.StatusServiceUnavailable)
	for n := 1; n <= 3; n++ {
		status, doc := discovery(t, idp)
		want := http.StatusServiceUnavailable
		if n == 3 {
			want = http.StatusOK
		}
		if status != want {
			t.Errorf("request %d: status %d, want %d (%v)", n, status, want, doc)
		}
		if n < 3 && doc["error"] != "discovery unavailable" {
			t.Errorf("request %d: body %v", n, doc)
		}
	}
	if got := idp.DiscoveryRequests(); got != 3 {
		t.Errorf("DiscoveryRequests = %d, want 3", got)
	}
	idp.FailDiscovery(0, http.StatusInternalServerError) // zero fails: serve
	if status, _ := discovery(t, idp); status != http.StatusOK {
		t.Errorf("FailDiscovery(0) status = %d", status)
	}
}

func TestUserinfoDefaultsAndOverrides(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	var info map[string]any
	if _, err := getJSON(idp.Issuer()+"/userinfo", &info); err != nil {
		t.Fatal(err)
	}
	if len(info) != 1 || info["sub"] != "fake-subject" {
		t.Errorf("default userinfo = %v", info)
	}
	idp.SetUserinfo(map[string]any{"sub": "u-9", "email": "u9@idp.invalid"})
	info = nil
	if _, err := getJSON(idp.Issuer()+"/userinfo", &info); err != nil {
		t.Fatal(err)
	}
	if info["sub"] != "u-9" || info["email"] != "u9@idp.invalid" {
		t.Errorf("userinfo = %v", info)
	}
}

func TestNewStopsTheServerWhenTheTestEnds(t *testing.T) {
	var issuer string
	t.Run("inner", func(t *testing.T) { issuer = fakeidp.New(t, clientID).Issuer() })
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(issuer + "/jwks")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the server still answers after its test ended: %d", resp.StatusCode)
	}
}

func TestSignClaimsRoundTripsAndReportsBadClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fakeidp.SignClaims(key, map[string]any{"sub": "s"})
	if err != nil {
		t.Fatal(err)
	}
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := jws.Verify(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil || claims["sub"] != "s" {
		t.Errorf("claims = %v (err %v)", claims, err)
	}
	if typ := jws.Signatures[0].Header.ExtraHeaders["typ"]; typ != "JWT" {
		t.Errorf("typ = %v, want JWT", typ)
	}
	_, err = fakeidp.SignClaims(key, map[string]any{"bad": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "fakeidp: claims") {
		t.Errorf("unmarshalable claims: err = %v", err)
	}
}
