package cloudtel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var gcpNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func TestGCPTokenServiceAccountSignsAndCaches(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	opts := f.authOptions()
	opts.CredentialsFile = f.serviceAccountFile(t)
	src, err := NewGCPTokenSource(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewGCPTokenSource: %v", err)
	}
	if src.Kind() != "service_account" || src.Project() != "proj-1" {
		t.Fatalf("kind/project = %q/%q", src.Kind(), src.Project())
	}
	tok, err := src.Token(context.Background())
	if err != nil || tok != "ya29.token-1" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	again, _ := src.Token(context.Background())
	if again != tok || f.called("token") != 1 {
		t.Fatalf("cached token = %q after %d token calls", again, f.called("token"))
	}
	form := f.tokenForms[0]
	if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant = %q", form.Get("grant_type"))
	}
}

func TestGCPTokenRefreshesBeforeExpiry(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	now := gcpNow
	opts := f.authOptions()
	opts.CredentialsFile = f.serviceAccountFile(t)
	opts.Now = func() time.Time { return now }
	src, err := NewGCPTokenSource(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = gcpNow.Add(59*time.Minute + 30*time.Second) // inside the refresh margin
	f.now = now
	tok, err := src.Token(context.Background())
	if err != nil || tok != "ya29.token-2" {
		t.Fatalf("refreshed token = %q, %v (calls %d)", tok, err, f.called("token"))
	}
}

func TestGCPTokenAuthorizedUserRefreshGrant(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	opts := f.authOptions()
	opts.WellKnownFile = writeJSON(t, "adc.json", map[string]string{
		"type": "authorized_user", "client_id": "cid", "client_secret": "csecret-x",
		"refresh_token": "rtoken-never-logged", "quota_project_id": "proj-q"})
	src, err := NewGCPTokenSource(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token(context.Background())
	if err != nil || tok != "ya29.token-1" || src.Kind() != "authorized_user" ||
		src.Project() != "proj-q" {
		t.Fatalf("tok=%q err=%v kind=%q project=%q", tok, err, src.Kind(), src.Project())
	}
	form := f.tokenForms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") !=
		"rtoken-never-logged" || form.Get("client_id") != "cid" {
		t.Fatalf("refresh form = %v", form)
	}
}

func TestGCPTokenMetadataServer(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	opts := f.authOptions()
	opts.MetadataHost = strings.TrimPrefix(f.srv.URL, "http://")
	src, err := NewGCPTokenSource(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token(context.Background())
	if err != nil || !strings.HasPrefix(tok, "ya29.meta-") || src.Kind() != "metadata" ||
		src.Project() != "proj-meta" {
		t.Fatalf("tok=%q err=%v kind=%q project=%q", tok, err, src.Kind(), src.Project())
	}
}

func TestGCPTokenNoCredentialsIsUnavailable(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	f.metadataUp = false
	opts := f.authOptions()
	opts.MetadataHost = strings.TrimPrefix(f.srv.URL, "http://")
	_, err := NewGCPTokenSource(context.Background(), opts)
	if !errors.Is(err, ErrNoCredentials) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	_, err = NewGCPTokenSource(context.Background(), f.authOptions())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("all sources disabled: err = %v", err)
	}
}

// An explicitly named credentials file that is unusable is an error, not a
// silent fall-through to another identity.
func TestGCPTokenExplicitFileProblems(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	cases := []struct {
		name string
		file string
		want error
	}{
		{"missing file", "/nonexistent/creds.json", ErrNoCredentials},
		{"not json", writeJSON(t, "bad.json", "]["), ErrMalformed},
		{"external account", writeJSON(t, "ext.json", map[string]string{
			"type": "external_account", "audience": "x"}), ErrUnavailable},
		{"bad private key", writeJSON(t, "badkey.json", map[string]string{
			"type": "service_account", "client_email": "a@b", "private_key": "nope",
			"token_uri": f.srv.URL + "/token"}), ErrMalformed},
		{"unknown type", writeJSON(t, "x.json", map[string]string{"type": "magic"}),
			ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := f.authOptions()
			opts.CredentialsFile = tc.file
			_, err := NewGCPTokenSource(context.Background(), opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGCPTokenEndpointErrors(t *testing.T) {
	cases := []struct {
		name string
		fail fakeFailure
		want error
	}{
		{"invalid client", fakeFailure{status: 401, code: "invalid_client",
			message: "The OAuth client was not found."}, ErrAuth},
		{"revoked grant", fakeFailure{status: 400, code: "invalid_grant",
			message: "Token has been expired or revoked."}, ErrAuth},
		{"clock skew", fakeFailure{status: 400, code: "invalid_grant",
			message: "Invalid JWT: Token must be a short-lived token (60 minutes) and in a " +
				"reasonable timeframe. Check your iat and exp values in the JWT claim."},
			ErrClockSkew},
		{"throttled", fakeFailure{status: 429, code: "rate_limited", message: "slow"},
			ErrThrottled},
		{"malformed", fakeFailure{status: 200, body: "{not json"}, ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGCP(t, gcpNow)
			f.fail["token"] = tc.fail
			opts := f.authOptions()
			opts.CredentialsFile = f.serviceAccountFile(t)
			src, err := NewGCPTokenSource(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			_, err = src.Token(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// The signed assertion uses the local clock; a skewed clock is reported
// as clock skew by the token endpoint and classified so.
func TestGCPTokenSkewedLocalClock(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	opts := f.authOptions()
	opts.CredentialsFile = f.serviceAccountFile(t)
	opts.Now = func() time.Time { return gcpNow.Add(-2 * time.Hour) }
	src, err := NewGCPTokenSource(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(context.Background()); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("err = %v, want ErrClockSkew", err)
	}
}

func TestGCPTokenErrorsNeverLeakSecrets(t *testing.T) {
	f := newFakeGCP(t, gcpNow)
	f.fail["token"] = fakeFailure{status: 400, code: "invalid_grant",
		message: "bad refresh"}
	opts := f.authOptions()
	opts.WellKnownFile = writeJSON(t, "adc.json", map[string]string{
		"type": "authorized_user", "client_id": "cid", "client_secret": "csecret-x",
		"refresh_token": "rtoken-never-logged"})
	src, err := NewGCPTokenSource(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"rtoken-never-logged", "csecret-x"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks %q: %v", secret, err)
		}
	}
}
