package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// G6-B04 / SURF-02: OIDC userinfo must carry a subject and a verified
// email; identities are keyed on issuer+subject.

func oidcProviderWithUserinfo(
	t *testing.T, payload map[string]any,
) *OAuthProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
		}))
	t.Cleanup(srv.Close)
	p := NewOAuthProvider(&config.OAuthConfig{
		Provider: "oidc", IssuerURL: "https://idp.example.com/",
	})
	p.discovery = &OIDCDiscovery{UserinfoEndpoint: srv.URL}
	return p
}

func TestFetchOIDCIdentity_RequiresVerifiedEmail(t *testing.T) {
	rejected := []map[string]any{
		{"sub": "s1", "email": "a@example.com", "email_verified": false},
		{"sub": "s1", "email": "a@example.com"},
		{"sub": "s1", "email": "a@example.com", "email_verified": "false"},
		{"sub": "s1", "email": "a@example.com", "email_verified": "yes"},
	}
	for _, payload := range rejected {
		p := oidcProviderWithUserinfo(t, payload)
		_, err := p.fetchOIDCIdentity(context.Background(), "tok")
		if !errors.Is(err, ErrOAuthEmailUnverified) {
			t.Errorf("payload %v: err = %v, want %v",
				payload, err, ErrOAuthEmailUnverified)
		}
	}
}

func TestFetchOIDCIdentity_RequiresSubject(t *testing.T) {
	p := oidcProviderWithUserinfo(t, map[string]any{
		"email": "a@example.com", "email_verified": true,
	})
	_, err := p.fetchOIDCIdentity(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Fatalf("err = %v, want missing subject error", err)
	}
}

func TestFetchOIDCIdentity_ReturnsIssuerSubjectEmail(t *testing.T) {
	for _, verified := range []any{true, "true"} {
		p := oidcProviderWithUserinfo(t, map[string]any{
			"sub": "subject-42", "email": "a@example.com",
			"email_verified": verified,
		})
		id, err := p.fetchOIDCIdentity(context.Background(), "tok")
		if err != nil {
			t.Fatalf("verified=%v: %v", verified, err)
		}
		want := Identity{
			Issuer: "https://idp.example.com", Subject: "subject-42",
			Email: "a@example.com", EmailVerified: true,
		}
		if !reflect.DeepEqual(id, want) {
			t.Errorf("identity = %+v, want %+v", id, want)
		}
	}
}

// G6-B11: the authenticated email must not be logged at INFO.
func TestFetchOIDCIdentity_DoesNotLogEmail(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := oidcProviderWithUserinfo(t, map[string]any{
		"sub": "s-log", "email": "secret-person@example.com",
		"email_verified": true,
	})
	if _, err := p.fetchOIDCIdentity(context.Background(), "tok"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if strings.Contains(buf.String(), "secret-person@example.com") {
		t.Fatalf("log output contains the raw email: %s", buf.String())
	}
}
