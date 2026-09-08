// Package providerobs reads provider evidence without granting database privileges.
package providerobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

var projectReference = regexp.MustCompile(`^[a-z0-9]+$`)

// Supabase uses only the Management API analytics_logs_read permission.
type Supabase struct {
	project string
	token   string
	http    *http.Client
}

func NewSupabase(project, token string) (*Supabase, error) {
	if !projectReference.MatchString(project) || strings.TrimSpace(token) == "" ||
		strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("Supabase observability requires a project ref and analytics read token")
	}
	return &Supabase{project: project, token: token, http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Supabase) get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u := "https://api.supabase.com/v1/projects/" + c.project + "/analytics/endpoints/" + endpoint
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("construct Supabase observability request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Supabase observability request failed; check network connectivity")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Supabase observability HTTP %d; check access and rate limits",
			resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("read Supabase observability response")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("Supabase response exceeds size limit")
	}
	return body, nil
}

// Metrics returns provider exposition; callers must verify measurement freshness and units.
func (c *Supabase) Metrics(ctx context.Context) (string, error) {
	body, err := c.get(ctx, "metrics", nil)
	return string(body), err
}
