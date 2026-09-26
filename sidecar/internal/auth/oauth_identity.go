package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Identity is an external login keyed on issuer+subject. Email is
// informational and is only trusted when EmailVerified is true.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
}

var (
	// ErrOAuthEmailUnverified is returned when the identity provider
	// does not assert email_verified=true (G6-B04 / SURF-02).
	ErrOAuthEmailUnverified = errors.New("oauth: email is not verified")
	// ErrOAuthLinkRequired is returned when a first login's email
	// belongs to an existing account that is not bound to this
	// issuer+subject. Such accounts are never auto-linked.
	ErrOAuthLinkRequired = errors.New(
		"oauth: email belongs to an existing account; " +
			"an administrator must link it explicitly")
)

const githubIssuer = "https://github.com"

func (p *OAuthProvider) fetchIdentity(
	ctx context.Context, accessToken string,
) (Identity, error) {
	if p.cfg.Provider == "github" {
		return p.fetchGitHubIdentity(ctx, accessToken)
	}
	return p.fetchOIDCIdentity(ctx, accessToken)
}

// issuer returns the configured issuer without a trailing slash.
func (p *OAuthProvider) issuer() string {
	if p.cfg.Provider == "google" {
		return "https://accounts.google.com"
	}
	return strings.TrimRight(p.cfg.IssuerURL, "/")
}

func (p *OAuthProvider) getJSON(
	ctx context.Context, endpoint, accessToken, what string, target any,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("oauth: building %s request: %w", what, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: %s request failed: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: %s returned status %d",
			what, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("oauth: decoding %s: %w", what, err)
	}
	return nil
}

func (p *OAuthProvider) fetchOIDCIdentity(
	ctx context.Context, accessToken string,
) (Identity, error) {
	endpoint := p.discovery.UserinfoEndpoint
	if endpoint == "" {
		return Identity{}, fmt.Errorf("oauth: no userinfo endpoint")
	}
	var info struct {
		Subject       string          `json:"sub"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
	}
	if err := p.getJSON(ctx, endpoint, accessToken,
		"userinfo", &info); err != nil {
		return Identity{}, err
	}
	if strings.TrimSpace(info.Subject) == "" {
		return Identity{}, fmt.Errorf(
			"oauth: no subject (sub) in userinfo response")
	}
	if info.Email == "" {
		return Identity{}, fmt.Errorf("oauth: no email in userinfo response")
	}
	if !verifiedClaim(info.EmailVerified) {
		return Identity{}, ErrOAuthEmailUnverified
	}
	return Identity{
		Issuer: p.issuer(), Subject: info.Subject,
		Email: info.Email, EmailVerified: true,
	}, nil
}

// verifiedClaim accepts the boolean true and the string "true" (some
// providers serialise booleans as strings). Anything else is false.
func verifiedClaim(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "true" || s == `"true"`
}

func (p *OAuthProvider) fetchGitHubIdentity(
	ctx context.Context, accessToken string,
) (Identity, error) {
	var user struct {
		ID    json.Number `json:"id"`
		Email string      `json:"email"`
	}
	if err := p.getJSON(ctx, "https://api.github.com/user", accessToken,
		"github /user", &user); err != nil {
		return Identity{}, err
	}
	if user.ID == "" || user.ID == "0" {
		return Identity{}, fmt.Errorf("oauth: github user has no id")
	}
	email := user.Email
	if email == "" {
		// GitHub only exposes a verified address as the public email;
		// otherwise require the verified primary address.
		var err error
		email, err = p.fetchGitHubEmailsFallback(ctx, accessToken)
		if err != nil {
			return Identity{}, err
		}
	}
	return Identity{
		Issuer: githubIssuer, Subject: user.ID.String(),
		Email: email, EmailVerified: true,
	}, nil
}

func (p *OAuthProvider) fetchGitHubEmailsFallback(
	ctx context.Context, accessToken string,
) (string, error) {
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := p.getJSON(ctx, "https://api.github.com/user/emails",
		accessToken, "github /user/emails", &emails); err != nil {
		return "", err
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}
	return "", fmt.Errorf("oauth: no verified primary email on github")
}
