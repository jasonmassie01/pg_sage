// Package azure applies server parameters on Azure Database for PostgreSQL
// flexible server through Azure Resource Manager, where ALTER SYSTEM is
// unavailable. It implements executor.ManagedConfigAdapter.
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

const (
	defaultBaseURL = "https://management.azure.com"
	apiVersion     = "2022-12-01"
	// ManagementScope is the OAuth scope for Azure Resource Manager.
	ManagementScope = "https://management.azure.com/.default"
)

// Server identifies one flexible server.
type Server struct {
	SubscriptionID string
	ResourceGroup  string
	Name           string
}

// TokenSource returns an ARM bearer token.
type TokenSource func(context.Context) (string, error)

// ParameterAdapter sets flexible-server configuration parameters.
type ParameterAdapter struct {
	server       Server
	token        TokenSource
	client       *http.Client
	baseURL      string
	pollInterval time.Duration
	pollTimeout  time.Duration
}

// Option configures a ParameterAdapter.
type Option func(*ParameterAdapter)

// WithBaseURL points the adapter at another ARM endpoint (tests, sovereign
// clouds such as https://management.usgovcloudapi.net).
func WithBaseURL(url string) Option {
	return func(a *ParameterAdapter) { a.baseURL = strings.TrimRight(url, "/") }
}

// WithPolling bounds how long an asynchronous ARM operation is awaited.
func WithPolling(interval, timeout time.Duration) Option {
	return func(a *ParameterAdapter) { a.pollInterval, a.pollTimeout = interval, timeout }
}

var (
	armNamePattern   = regexp.MustCompile(`^[A-Za-z0-9._()-]{1,90}$`)
	parameterPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
)

// NewParameterAdapter validates the server identity and returns an adapter.
func NewParameterAdapter(server Server, token TokenSource, opts ...Option) (*ParameterAdapter, error) {
	for label, value := range map[string]string{
		"subscription": server.SubscriptionID, "resource group": server.ResourceGroup,
		"server name": server.Name,
	} {
		if !armNamePattern.MatchString(value) || strings.Contains(value, "..") {
			return nil, fmt.Errorf("azure: invalid %s %q", label, value)
		}
	}
	if token == nil {
		return nil, errors.New("azure: a token source is required")
	}
	adapter := &ParameterAdapter{
		server: server, token: token, client: &http.Client{Timeout: 30 * time.Second},
		baseURL: defaultBaseURL, pollInterval: 2 * time.Second, pollTimeout: 5 * time.Minute,
	}
	for _, opt := range opts {
		opt(adapter)
	}
	return adapter, nil
}

type configProps struct {
	Value                  string `json:"value,omitempty"`
	DefaultValue           string `json:"defaultValue,omitempty"`
	Unit                   string `json:"unit,omitempty"`
	Source                 string `json:"source,omitempty"`
	IsReadOnly             bool   `json:"isReadOnly,omitempty"`
	IsDynamicConfig        bool   `json:"isDynamicConfig,omitempty"`
	IsConfigPendingRestart bool   `json:"isConfigPendingRestart,omitempty"`
}

type configResource struct {
	Properties configProps `json:"properties"`
}

// ApplyParameter sets (or resets) one server parameter and reports whether
// it is in effect or waits for a server restart.
func (a *ParameterAdapter) ApplyParameter(
	ctx context.Context, change executor.ManagedConfigChange,
) (executor.ManagedConfigResult, error) {
	if change.Mechanism != executor.ManagedServerParameter {
		return executor.ManagedConfigResult{}, fmt.Errorf(
			"azure: mechanism %q is not server_parameter", change.Mechanism)
	}
	if !parameterPattern.MatchString(change.Parameter) {
		return executor.ManagedConfigResult{}, fmt.Errorf(
			"azure: invalid parameter name %q", change.Parameter)
	}
	current, err := a.getParameter(ctx, change.Parameter)
	if err != nil {
		return executor.ManagedConfigResult{}, err
	}
	target, source, err := targetValue(change, current)
	if err != nil {
		return executor.ManagedConfigResult{}, err
	}
	if err := a.putParameter(ctx, change.Parameter, target, source); err != nil {
		return executor.ManagedConfigResult{}, err
	}
	after, err := a.getParameter(ctx, change.Parameter)
	if err != nil {
		return executor.ManagedConfigResult{}, err
	}
	return appliedResult(change.Parameter, target, after), nil
}

func targetValue(change executor.ManagedConfigChange, current configProps) (string, string, error) {
	if current.IsReadOnly {
		return "", "", fmt.Errorf("%w: azure parameter %s is read-only",
			executor.ErrManagedConfigUnsupported, change.Parameter)
	}
	if change.Reset {
		return current.DefaultValue, "system-default", nil
	}
	value, err := convertToUnit(change.Value, current.Unit)
	if err != nil {
		return "", "", fmt.Errorf("azure parameter %s: %w", change.Parameter, err)
	}
	return value, "user-override", nil
}

func appliedResult(parameter, target string, after configProps) executor.ManagedConfigResult {
	switch {
	case after.IsConfigPendingRestart:
		return executor.ManagedConfigResult{Note: fmt.Sprintf(
			"azure parameter %s set to %s; takes effect after a server restart", parameter, target)}
	case after.Value != target:
		return executor.ManagedConfigResult{Note: fmt.Sprintf(
			"azure parameter %s reports %q after setting %q", parameter, after.Value, target)}
	default:
		return executor.ManagedConfigResult{InEffect: true, Note: fmt.Sprintf(
			"azure parameter %s set to %s", parameter, target)}
	}
}

func (a *ParameterAdapter) parameterURL(parameter string) string {
	return fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/providers/"+
		"Microsoft.DBforPostgreSQL/flexibleServers/%s/configurations/%s?api-version=%s",
		a.baseURL, a.server.SubscriptionID, a.server.ResourceGroup, a.server.Name,
		parameter, apiVersion)
}

func (a *ParameterAdapter) getParameter(ctx context.Context, parameter string) (configProps, error) {
	var resource configResource
	_, err := a.do(ctx, http.MethodGet, a.parameterURL(parameter), nil, &resource)
	if err != nil {
		return configProps{}, fmt.Errorf("azure: read parameter %s: %w", parameter, err)
	}
	return resource.Properties, nil
}

func (a *ParameterAdapter) putParameter(ctx context.Context, parameter, value, source string) error {
	body := configResource{Properties: configProps{Value: value, Source: source}}
	resp, err := a.do(ctx, http.MethodPut, a.parameterURL(parameter), body, nil)
	if err != nil {
		return fmt.Errorf("azure: set parameter %s: %w", parameter, err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil
	}
	operation := resp.Header.Get("Azure-AsyncOperation")
	if operation == "" {
		operation = resp.Header.Get("Location")
	}
	if operation == "" {
		return fmt.Errorf("azure: set parameter %s: accepted without an operation URL", parameter)
	}
	return a.awaitOperation(ctx, operation)
}

type operationStatus struct {
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *ParameterAdapter) awaitOperation(ctx context.Context, operationURL string) error {
	deadline := time.Now().Add(a.pollTimeout)
	for {
		var status operationStatus
		if _, err := a.do(ctx, http.MethodGet, operationURL, nil, &status); err != nil {
			return fmt.Errorf("azure: poll operation: %w", err)
		}
		switch strings.ToLower(status.Status) {
		case "succeeded":
			return nil
		case "failed", "canceled", "cancelled":
			if status.Error != nil {
				return fmt.Errorf("azure: operation %s: %s: %s",
					status.Status, status.Error.Code, status.Error.Message)
			}
			return fmt.Errorf("azure: operation %s", status.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("azure: operation still %q after %s", status.Status, a.pollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.pollInterval):
		}
	}
}

type armError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do sends one authenticated ARM request and decodes a 2xx JSON response.
func (a *ParameterAdapter) do(
	ctx context.Context, method, url string, body, out any,
) (*http.Response, error) {
	token, err := a.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire ARM token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, armStatusError(resp.StatusCode, raw)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode ARM response: %w", err)
		}
	}
	return resp, nil
}

func armStatusError(status int, raw []byte) error {
	var parsed armError
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		return fmt.Errorf("ARM %d %s: %s", status, parsed.Error.Code, parsed.Error.Message)
	}
	return fmt.Errorf("ARM %d: %s", status, strings.TrimSpace(string(raw)))
}
