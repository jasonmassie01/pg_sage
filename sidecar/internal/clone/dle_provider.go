package clone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type DLEConfig struct {
	Endpoint   string
	Token      string
	HTTPClient *http.Client
	Now        func() time.Time
}

type DLEProvider struct {
	endpoint string
	token    string
	client   *http.Client
	now      func() time.Time
}

func NewDLEProvider(cfg DLEConfig) (*DLEProvider, error) {
	parsed, err := url.ParseRequestURI(cfg.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("DLE endpoint must be an absolute URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("DLE token is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &DLEProvider{strings.TrimRight(cfg.Endpoint, "/"), cfg.Token, client, now}, nil
}

func (p *DLEProvider) Create(ctx context.Context, spec CloneSpec) (Clone, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return Clone{}, fmt.Errorf("encode DLE create request: %w", err)
	}
	request, err := p.request(ctx, http.MethodPost, "/clones", bytes.NewReader(body))
	if err != nil {
		return Clone{}, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return Clone{}, fmt.Errorf("DLE create request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return Clone{}, fmt.Errorf("DLE create failed with status %d", response.StatusCode)
	}
	var result Clone
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return Clone{}, fmt.Errorf("decode DLE create response: %w", err)
	}
	if result.ID == "" || result.DSN == "" || result.CreatedFrom.IsZero() {
		return Clone{}, p.cleanupInvalid(ctx, result)
	}
	return result, nil
}

func (p *DLEProvider) cleanupInvalid(ctx context.Context, partial Clone) error {
	base := errors.New("invalid clone response")
	if partial.ID == "" {
		return base
	}
	if err := p.Destroy(ctx, partial); err != nil {
		return fmt.Errorf("%w; cleanup failed: %v", base, err)
	}
	return base
}

func (p *DLEProvider) Destroy(ctx context.Context, target Clone) error {
	if target.ID == "" {
		return errors.New("clone ID is required")
	}
	request, err := p.request(ctx, http.MethodDelete,
		"/clones/"+url.PathEscape(target.ID), nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("DLE destroy request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("DLE destroy failed with status %d", response.StatusCode)
	}
	return nil
}

func (p *DLEProvider) SnapshotAge(ctx context.Context) (time.Duration, error) {
	request, err := p.request(ctx, http.MethodGet, "/snapshots/latest", nil)
	if err != nil {
		return 0, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("DLE snapshot request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("DLE snapshot failed with status %d", response.StatusCode)
	}
	var payload struct {
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode DLE snapshot response: %w", err)
	}
	if payload.CreatedAt.IsZero() {
		return 0, errors.New("DLE snapshot timestamp is missing")
	}
	age := p.now().Sub(payload.CreatedAt)
	if age < 0 {
		return 0, errors.New("DLE snapshot timestamp is in the future")
	}
	return age, nil
}

func (p *DLEProvider) request(
	ctx context.Context, method, path string, body *bytes.Reader,
) (*http.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var request *http.Request
	var err error
	if body == nil {
		request, err = http.NewRequestWithContext(ctx, method, p.endpoint+path, nil)
	} else {
		request, err = http.NewRequestWithContext(ctx, method, p.endpoint+path, body)
		request.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		return nil, fmt.Errorf("build DLE request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	return request, nil
}
