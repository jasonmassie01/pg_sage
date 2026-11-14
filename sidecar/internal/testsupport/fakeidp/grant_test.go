package fakeidp_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/fakeidp"
)

func wantGrantError(t *testing.T, name string, status int, body map[string]any) {
	t.Helper()
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("%s: status %d body %v, want 400 invalid_grant", name, status, body)
	}
}

// Invalid input: the token endpoint enforces PKCE S256 and single-use codes.
func TestTokenEndpointRejectsBadGrants(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	cases := []struct {
		name, method, verifier string
		ok                     bool
	}{
		{"wrong verifier", "S256", "not-the-verifier", false},
		{"missing verifier", "S256", "", false},
		{"plain method", "plain", "", false},
		{"verifier without a challenge", "", "unexpected-verifier", false},
		{"no PKCE at all", "", "", true},
	}
	for n, tc := range cases {
		l := newLogin(n)
		code, err := idp.Authorize(authorizeURL(idp, l, tc.method))
		if err != nil {
			t.Fatal(err)
		}
		verifier := tc.verifier
		if tc.method == "plain" {
			verifier = l.verifier
		}
		status, body, err := redeem(idp, code, verifier)
		if err != nil {
			t.Fatal(err)
		}
		if tc.ok {
			if status != http.StatusOK || body["id_token"] == nil {
				t.Errorf("%s: status %d body %v, want a token", tc.name, status, body)
			}
			continue
		}
		wantGrantError(t, tc.name, status, body)
	}
	status, body, err := redeem(idp, "code-never-issued", "whatever")
	if err != nil {
		t.Fatal(err)
	}
	wantGrantError(t, "unknown code", status, body)
}

func TestCodesAreSingleUse(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	l := newLogin(1)
	code, err := idp.Authorize(authorizeURL(idp, l, "S256"))
	if err != nil {
		t.Fatal(err)
	}
	if code != "code-"+l.state {
		t.Errorf("code = %q, want code-%s", code, l.state)
	}
	if status, body, err := redeem(idp, code, l.verifier); err != nil || status != 200 {
		t.Fatalf("first redemption: %d %v %v", status, body, err)
	}
	status, body, err := redeem(idp, code, l.verifier)
	if err != nil {
		t.Fatal(err)
	}
	wantGrantError(t, "second redemption", status, body)

	// A failed redemption burns the code too: the right verifier after a
	// wrong one is refused.
	l2 := newLogin(2)
	code2, err := idp.Authorize(authorizeURL(idp, l2, "S256"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := redeem(idp, code2, "wrong"); err != nil {
		t.Fatal(err)
	}
	status, body, err = redeem(idp, code2, l2.verifier)
	if err != nil {
		t.Fatal(err)
	}
	wantGrantError(t, "redemption after a failed one", status, body)
}

func TestTokenEndpointRejectsAMalformedForm(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	resp, err := http.Post(idp.Issuer()+"/token", "application/x-www-form-urlencoded",
		strings.NewReader("code=%zz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if got := len(idp.TokenRequests()); got != 0 {
		t.Errorf("a malformed form was recorded: %d requests", got)
	}
}

func TestAuthorizeRejectsAMalformedURL(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	code, err := idp.Authorize("http://[::1")
	if err == nil || code != "" ||
		!strings.Contains(err.Error(), "fakeidp: parsing authorize url") {
		t.Errorf("code %q err %v, want the parse error", code, err)
	}
}

func TestTokenRequestsRecordsEachFormAsACopy(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	if got := idp.TokenRequests(); len(got) != 0 {
		t.Fatalf("a fresh IdP has %d token requests", len(got))
	}
	mustSignIn(t, idp, newLogin(1))
	if _, _, err := redeem(idp, "code-unknown", ""); err != nil {
		t.Fatal(err)
	}
	got := idp.TokenRequests()
	if len(got) != 2 || got[0].Get("code") != "code-state-01" ||
		got[1].Get("code") != "code-unknown" {
		t.Fatalf("token requests = %v", got)
	}
	if got[0].Get("code_verifier") != newLogin(1).verifier {
		t.Errorf("verifier not recorded: %v", got[0])
	}
	got[0] = url.Values{}
	if again := idp.TokenRequests(); again[0].Get("code") != "code-state-01" {
		t.Error("changing the returned slice changed the recorded requests")
	}
}

// Concurrency: 16 parallel sign-ins, with setters racing them, each get a
// token carrying their own nonce (run under -race in the Linux runner).
func TestParallelSignInsKeepTheirOwnNonce(t *testing.T) {
	idp := fakeidp.New(t, clientID)
	const n = 16
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := newLogin(i)
			body, err := signIn(idp, l)
			if err != nil {
				errs <- err
				return
			}
			_, claims, err := verifyIDToken(idp, body["id_token"].(string))
			if err != nil {
				errs <- err
				return
			}
			if claims["nonce"] != l.nonce || claims["sub"] != "fake-subject" {
				errs <- fmt.Errorf("login %d: claims %v", i, claims)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			idp.SetUserinfo(map[string]any{"sub": fmt.Sprint(i)})
			idp.SetDiscovery(map[string]any{"x-round": i})
			_ = idp.DiscoveryRequests()
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(idp.TokenRequests()); got != n {
		t.Errorf("token requests = %d, want %d", got, n)
	}
}
