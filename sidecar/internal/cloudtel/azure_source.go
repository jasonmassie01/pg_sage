package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// Azure Database for PostgreSQL flexible server host telemetry from Azure
// Resource Manager: the server (SKU, storage auto-grow) and its replicas,
// and Microsoft.Insights/metrics of the server and of each replica. Reads
// only; the credential is the ARM token chain (azure.DefaultTokenSource).

const (
	azureServerAPIVersion  = "2022-12-01"
	azureMetricsAPIVersion = "2018-01-01"
)

// AzureTokenFunc returns an ARM bearer token (azure.TokenSource converts).
type AzureTokenFunc func(ctx context.Context) (string, error)

// AzureOptions identify one flexible server.
type AzureOptions struct {
	SubscriptionID, ResourceGroup, Server string
	Token                                 AzureTokenFunc
	HTTPClient                            *http.Client
	Endpoint                              string // ARM; tests and sovereign clouds
	Now                                   func() time.Time
	Sleep                                 func(ctx context.Context, d time.Duration) error
}

var (
	azureSubscriptionPattern = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	azureGroupPattern  = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	azureServerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	// azureServerIDPattern is a flexible server's resource ID (a replica's).
	azureServerIDPattern = regexp.MustCompile(`(?i)^/subscriptions/([^/]+)/resourceGroups/` +
		`([^/]+)/providers/Microsoft\.DBforPostgreSQL/flexibleServers/([^/]+)$`)
)

// AzureSource collects telemetry for one flexible server.
type AzureSource struct{ opts AzureOptions }

// NewAzureSource validates the identity; it makes no network call.
func NewAzureSource(opts AzureOptions) (*AzureSource, error) {
	opts.SubscriptionID = strings.ToLower(opts.SubscriptionID)
	switch {
	case !azureSubscriptionPattern.MatchString(opts.SubscriptionID):
		return nil, fmt.Errorf("%w: invalid Azure subscription %q", ErrIdentity,
			opts.SubscriptionID)
	case !azureGroupPattern.MatchString(opts.ResourceGroup) ||
		strings.Contains(opts.ResourceGroup, ".."):
		return nil, fmt.Errorf("%w: invalid Azure resource group %q", ErrIdentity,
			opts.ResourceGroup)
	case !azureServerPattern.MatchString(opts.Server):
		return nil, fmt.Errorf("%w: invalid flexible server name %q", ErrIdentity, opts.Server)
	case opts.Token == nil:
		return nil, fmt.Errorf("%w: no Azure token source", ErrNoCredentials)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = egress.HTTPClient(20 * time.Second)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Endpoint == "" {
		opts.Endpoint = "https://management.azure.com"
	}
	opts.Endpoint = strings.TrimRight(opts.Endpoint, "/")
	return &AzureSource{opts: opts}, nil
}

// Provider is "azure".
func (s *AzureSource) Provider() string { return "azure" }

func azureServerID(subscription, group, server string) string {
	return "/subscriptions/" + url.PathEscape(subscription) + "/resourceGroups/" +
		url.PathEscape(group) + "/providers/Microsoft.DBforPostgreSQL/flexibleServers/" +
		url.PathEscape(server)
}

func (s *AzureSource) serverID() string {
	return azureServerID(s.opts.SubscriptionID, s.opts.ResourceGroup, s.opts.Server)
}

// get sends an authorized ARM GET and returns a 200 body; a throttled
// read is retried inside the poll. role names what a denial lacks.
func (s *AzureSource) get(ctx context.Context, path string, q url.Values,
	api, role string) ([]byte, error) {
	return retryThrottled(ctx, s.opts.Sleep, func() ([]byte, time.Duration, error) {
		return s.getOnce(ctx, s.opts.Endpoint+path+"?"+q.Encode(), api, role)
	})
}

func (s *AzureSource) getOnce(ctx context.Context, rawURL, api, role string) ([]byte,
	time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	token, err := s.opts.Token(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: azure credential: %s", ErrNoCredentials,
			sanitize(err.Error()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build %s request", ErrProvider, api)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("%w: %s unreachable; check network access", ErrProvider, api)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return nil, 0, fmt.Errorf("%w: %s response unreadable or too large", ErrMalformed, api)
	}
	if err := checkServerDate(resp.Header.Get("Date"), s.opts.Now()); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", api, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, retryAfter(resp.Header.Get("Retry-After")),
			azureAPIError(api, role, resp.StatusCode, raw)
	}
	return raw, 0, nil
}

func azureAPIError(api, role string, status int, raw []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	detail := fmt.Sprintf("%s HTTP %d %s: %s", api, status, sanitize(e.Error.Code),
		sanitize(e.Error.Message))
	switch status {
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrThrottled, detail)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s; grant the credential the %s role", ErrAuth, detail, role)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrIdentity, detail)
	}
	return fmt.Errorf("%w: %s", ErrProvider, detail)
}
