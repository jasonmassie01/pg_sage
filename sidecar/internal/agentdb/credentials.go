package agentdb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// secretRefPattern allow-lists secret references to dedicated AgentDB
// environment variables so a deployment can never point the fleet collector
// at the meta-database DSN or another sidecar secret (G8-B14).
var secretRefPattern = regexp.MustCompile(`^env:(//)?PG_SAGE_AGENTDB_[A-Z0-9_]+$`)

// ValidateSecretRef accepts an empty reference or an allow-listed env ref.
func ValidateSecretRef(ref string) error {
	if ref == "" || secretRefPattern.MatchString(ref) {
		return nil
	}
	return fmt.Errorf("%w: secret_ref must be env:PG_SAGE_AGENTDB_<NAME>", ErrInvalid)
}

// supabaseProjectPassword derives a distinct database password per project
// from the configured master secret, so tenants never share a password yet
// an operator holding the master secret can recover it (G8-B19).
func supabaseProjectPassword(master, projectName string) string {
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte("pg_sage/supabase/db_pass/" + projectName))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// staticGCPToken wraps a static OAuth access token and reports its expiry
// instead of failing opaquely once Google rejects it (G8-B09, SURF-20).
type staticGCPToken struct {
	token    string
	issuedAt time.Time
	ttl      time.Duration
	now      func() time.Time
}

func newStaticGCPToken(token string, ttl time.Duration) *staticGCPToken {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &staticGCPToken{token: token, issuedAt: time.Now(), ttl: ttl, now: time.Now}
}

func (s *staticGCPToken) Token(context.Context) (string, error) {
	if err := s.CredentialError(s.now()); err != nil {
		return "", err
	}
	return s.token, nil
}

// CredentialError reports whether the static token is past its lifetime.
func (s *staticGCPToken) CredentialError(now time.Time) error {
	if now.Sub(s.issuedAt) < s.ttl {
		return nil
	}
	return providerError(ProviderGCPCloudSQL, ProviderErrPermission,
		fmt.Sprintf("static PG_SAGE_GCP_ACCESS_TOKEN is older than %s and has expired", s.ttl),
		"set PG_SAGE_GCP_TOKEN_SOURCE=metadata for a refreshing token, or restart with a new token")
}

// metadataGCPToken fetches and refreshes tokens from the GCE/GKE/Cloud Run
// metadata server (workload identity), caching until shortly before expiry.
type metadataGCPToken struct {
	baseURL string
	client  *http.Client
	mu      sync.Mutex
	token   string
	expires time.Time
}

func newMetadataGCPToken(baseURL string) *metadataGCPToken {
	if baseURL == "" {
		baseURL = "http://metadata.google.internal"
	}
	return &metadataGCPToken{baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{Timeout: 5 * time.Second}}
}

func (m *metadataGCPToken) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && time.Now().Before(m.expires) {
		return m.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+
		"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gcp metadata token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gcp metadata token: status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.AccessToken == "" {
		return "", fmt.Errorf("gcp metadata token: malformed response")
	}
	m.token = payload.AccessToken
	m.expires = time.Now().Add(time.Duration(max(payload.ExpiresIn-60, 0)) * time.Second)
	return m.token, nil
}

func (m *metadataGCPToken) CredentialError(time.Time) error { return nil }

type gcpTokenSource interface {
	Token(context.Context) (string, error)
	CredentialError(time.Time) error
}

// gcpTokenSourceFromEnv prefers the refreshing metadata-server source and
// falls back to a static token whose expiry is reported.
func gcpTokenSourceFromEnv() gcpTokenSource {
	if os.Getenv("PG_SAGE_GCP_TOKEN_SOURCE") == "metadata" {
		return newMetadataGCPToken(os.Getenv("PG_SAGE_GCP_METADATA_URL"))
	}
	token := os.Getenv("PG_SAGE_GCP_ACCESS_TOKEN")
	if token == "" {
		return nil
	}
	ttl, _ := strconv.Atoi(os.Getenv("PG_SAGE_GCP_ACCESS_TOKEN_TTL_SECONDS"))
	return newStaticGCPToken(token, time.Duration(ttl)*time.Second)
}

// LiveProviders lists the providers that have live runners, in display order.
func LiveProviders() []string {
	return []string{ProviderNeon, ProviderSupabase, ProviderAWSRDS,
		ProviderGCPCloudSQL, ProviderDatabricksLakebase}
}
