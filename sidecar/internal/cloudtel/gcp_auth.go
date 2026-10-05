package cloudtel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Google credentials from the standard Application Default Credentials
// chain: GOOGLE_APPLICATION_CREDENTIALS (service account key or user
// credentials), the gcloud ADC file, then the metadata server (GCE, GKE
// workload identity, Cloud Run). Tokens are cached until a minute before
// expiry and never logged.

const (
	gcpScope        = "https://www.googleapis.com/auth/cloud-platform"
	gcpTokenURL     = "https://oauth2.googleapis.com/token"
	refreshMargin   = time.Minute
	metadataTimeout = 2 * time.Second
)

// GCPAuthOptions select and isolate the credential chain.
type GCPAuthOptions struct {
	CredentialsFile string // explicit key file (GOOGLE_APPLICATION_CREDENTIALS)
	WellKnownFile   string // gcloud ADC file; "" = the OS default, "-" = skip
	MetadataHost    string // "" = $GCE_METADATA_HOST or metadata.google.internal; "-" = skip
	TokenURL        string // user-credential token endpoint (default Google's)
	HTTPClient      *http.Client
	Now             func() time.Time
}

type tokenFetch func(ctx context.Context) (token string, ttl time.Duration, err error)

// GCPToken is a cached, refreshing Google access token source.
type GCPToken struct {
	kind, project string
	fetch         tokenFetch
	now           func() time.Time
	mu            sync.Mutex
	token         string
	expires       time.Time
}

// Kind is service_account, authorized_user or metadata.
func (t *GCPToken) Kind() string { return t.kind }

// Project is the project the credentials name ("" when they name none).
func (t *GCPToken) Project() string { return t.project }

// Token returns a valid access token, refreshing it when needed.
func (t *GCPToken) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Add(refreshMargin).Before(t.expires) {
		return t.token, nil
	}
	tok, ttl, err := t.fetch(ctx)
	if err != nil {
		return "", err
	}
	t.token, t.expires = tok, t.now().Add(ttl)
	return tok, nil
}

// NewGCPTokenSource resolves the first credential the chain offers.
func NewGCPTokenSource(ctx context.Context, opts GCPAuthOptions) (*GCPToken, error) {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TokenURL == "" {
		opts.TokenURL = gcpTokenURL
	}
	if opts.CredentialsFile != "" {
		raw, err := os.ReadFile(opts.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("%w: GOOGLE_APPLICATION_CREDENTIALS file is not readable",
				ErrNoCredentials)
		}
		return credentialsFromJSON(raw, opts)
	}
	if path := wellKnownPath(opts.WellKnownFile); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			return credentialsFromJSON(raw, opts)
		}
	}
	if host := metadataHost(opts.MetadataHost); host != "" {
		if t, err := metadataSource(ctx, host, opts); err == nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: no Google credentials: set GOOGLE_APPLICATION_CREDENTIALS, "+
		"run gcloud auth application-default login, or run on GCE/GKE/Cloud Run with a "+
		"service account", ErrNoCredentials)
}

func wellKnownPath(opt string) string {
	switch {
	case opt == "-":
		return ""
	case opt != "":
		return opt
	case runtime.GOOS == "windows":
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "gcloud", "application_default_credentials.json")
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}

func metadataHost(opt string) string {
	switch {
	case opt == "-":
		return ""
	case opt != "":
		return opt
	case os.Getenv("GCE_METADATA_HOST") != "":
		return os.Getenv("GCE_METADATA_HOST")
	}
	return "metadata.google.internal"
}

type credentialFile struct {
	Type           string `json:"type"`
	ProjectID      string `json:"project_id"`
	QuotaProjectID string `json:"quota_project_id"`
	ClientEmail    string `json:"client_email"`
	PrivateKeyID   string `json:"private_key_id"`
	PrivateKey     string `json:"private_key"`
	TokenURI       string `json:"token_uri"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RefreshToken   string `json:"refresh_token"`
}

func credentialsFromJSON(raw []byte, opts GCPAuthOptions) (*GCPToken, error) {
	var f credentialFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%w: Google credentials file is not valid JSON", ErrMalformed)
	}
	switch f.Type {
	case "service_account":
		return serviceAccountSource(f, opts)
	case "authorized_user":
		if f.ClientID == "" || f.RefreshToken == "" {
			return nil, fmt.Errorf("%w: authorized_user credentials lack a client or "+
				"refresh token", ErrMalformed)
		}
		return &GCPToken{kind: "authorized_user", project: f.QuotaProjectID, now: opts.Now,
			fetch: func(ctx context.Context) (string, time.Duration, error) {
				form := url.Values{"grant_type": {"refresh_token"}, "client_id": {f.ClientID},
					"client_secret": {f.ClientSecret}, "refresh_token": {f.RefreshToken}}
				return postToken(ctx, opts.HTTPClient, opts.TokenURL, form)
			}}, nil
	case "external_account", "impersonated_service_account":
		return nil, fmt.Errorf("%w: %s credentials are not supported; use a service account "+
			"key, gcloud user credentials or the metadata server", ErrUnavailable, f.Type)
	}
	return nil, fmt.Errorf("%w: unknown Google credentials type %q", ErrMalformed,
		sanitize(f.Type))
}

// postToken exchanges a grant at a token endpoint.
func postToken(ctx context.Context, client *http.Client, endpoint string,
	form url.Values) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("%w: build token request", ErrProvider)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doToken(ctx, client, req, "Google token endpoint")
}

func doToken(ctx context.Context, client *http.Client, req *http.Request,
	what string) (string, time.Duration, error) {
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, fmt.Errorf("%w: %s unreachable", ErrProvider, what)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("%w: read %s response", ErrMalformed, what)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, tokenError(what, resp.StatusCode, raw)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return "", 0, fmt.Errorf("%w: %s returned no access token", ErrMalformed, what)
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return tok.AccessToken, ttl, nil
}

func tokenError(what string, status int, raw []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	detail := fmt.Sprintf("%s HTTP %d %s: %s", what, status, sanitize(e.Error),
		sanitize(e.Description))
	desc := strings.ToLower(e.Description)
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrThrottled, detail)
	case e.Error == "invalid_grant" && (strings.Contains(desc, "iat") ||
		strings.Contains(desc, "exp values") || strings.Contains(desc, "timeframe") ||
		strings.Contains(desc, "clock")):
		return fmt.Errorf("%w: %s; check the local clock (NTP)", ErrClockSkew, detail)
	case status >= 400 && status < 500:
		return fmt.Errorf("%w: %s", ErrAuth, detail)
	}
	return fmt.Errorf("%w: %s", ErrProvider, detail)
}

var errMetadataUnavailable = errors.New("metadata server unavailable")
