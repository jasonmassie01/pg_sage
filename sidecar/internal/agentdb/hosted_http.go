package agentdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HostedHTTPClient struct {
	Provider     string
	BaseURL      string
	TokenFunc    func(context.Context) (string, error)
	PasswordFunc func(context.Context) (string, error)
	HTTPClient   *http.Client
}

func (c HostedHTTPClient) request(
	ctx context.Context, method, path string, body any,
) (*http.Request, error) {
	if c.TokenFunc == nil {
		return nil, fmt.Errorf("provider management token is required")
	}
	token, err := c.TokenFunc(ctx)
	if err != nil {
		return nil, fmt.Errorf("load provider token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("provider management token is empty")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode provider request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	base := c.BaseURL
	if base == "" {
		if c.Provider == ProviderNeon {
			base = "https://console.neon.tech/api/v2"
		} else {
			base = "https://api.supabase.com/v1"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path,
		reader)
	if err != nil {
		return nil, fmt.Errorf("invalid provider request URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c HostedHTTPClient) do(
	ctx context.Context, method, path string, body any,
) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("provider transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, hostedHTTPError(c.Provider, response.StatusCode)
	}
	if response.StatusCode == http.StatusNoContent {
		return map[string]any{}, nil
	}
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		if method == http.MethodDelete && err == io.EOF {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("invalid provider JSON response")
	}
	return result, nil
}

func hostedHTTPError(provider string, status int) error {
	kind := ProviderErrUnavailable
	switch status {
	case 400:
		kind = ProviderErrInvalid
	case 401, 403:
		kind = ProviderErrPermission
	case 402:
		kind = ProviderErrQuota
	case 404:
		kind = ProviderErrNotFound
	case 409:
		kind = ProviderErrConflict
	case 429:
		kind = ProviderErrThrottle
	}
	return providerError(provider, kind, fmt.Sprintf("management API returned HTTP %d", status),
		"verify scoped credentials, resource state, quota and plan entitlement")
}
