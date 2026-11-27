package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// The sidecar API client every API-calling subcommand shares (plan-03a's
// plan verbs and cli-01a's plan status form, approve, apply and explain):
// it sends the operator API token only as `Authorization: Bearer`, names
// the surface (X-Sage-Surface: cli) so a decision is recorded as made
// from the CLI, bounds each call by a timeout, and never prints the token:
// a sidecar message that echoes it is redacted.

const (
	cliHTTPTimeout   = 30 * time.Second
	cliMaxAnswer     = 8 << 20
	cliSurfaceHeader = "X-Sage-Surface"
	cliRedacted      = "[redacted]"
)

// apiClient calls one sidecar's API with one token.
type apiClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

// apiAnswer is one sidecar answer: its status and decoded body.
type apiAnswer struct {
	status int
	raw    []byte
	body   map[string]any
}

// errCLIBuild is a request that could not be built (a client bug).
var errCLIBuild = errors.New("build the request")

// cliTransportError is a sidecar that could not be reached or read.
type cliTransportError struct {
	host string
	err  error
}

func (e *cliTransportError) Error() string {
	return fmt.Sprintf("cannot reach the sidecar at %s: %v", e.host, e.err)
}

// cliRefusal is a non-2xx answer: its status, code and message.
type cliRefusal struct {
	status    int
	code, msg string
}

func (e *cliRefusal) Error() string {
	return fmt.Sprintf("the sidecar refused (HTTP %d %s): %s", e.status, e.code, e.msg)
}

func newAPIClient(base *url.URL, token string, client *http.Client) *apiClient {
	if client == nil {
		// The egress floor refuses metadata and link-local destinations
		// on every dial and redirect.
		client = egress.HTTPClient(cliHTTPTimeout)
	}
	return &apiClient{base: base, token: token, http: client}
}

// send performs one request; a non-2xx answer is a *cliRefusal, a
// transport failure a *cliTransportError.
func (c *apiClient) send(ctx context.Context, method, path string, query url.Values,
	body any) (apiAnswer, error) {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return apiAnswer{}, fmt.Errorf("%w: %v", errCLIBuild, c.redact(err.Error()))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return apiAnswer{}, &cliTransportError{host: c.base.Host,
			err: errors.New(c.redact(transportCause(err)))}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cliMaxAnswer))
	if err != nil {
		return apiAnswer{}, &cliTransportError{host: c.base.Host,
			err: fmt.Errorf("read the answer: %s", c.redact(err.Error()))}
	}
	ans := apiAnswer{status: resp.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &ans.body) // a non-JSON answer is reported by its text
	if resp.StatusCode/100 == 2 {
		return ans, nil
	}
	msg, _ := ans.body["error"].(string)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	code, _ := ans.body["code"].(string)
	return ans, &cliRefusal{status: resp.StatusCode, code: code, msg: c.redact(msg)}
}

// transportCause is the transport error without the request URL, which
// net/http prefixes and which is already named by the host.
func transportCause(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err.Error()
	}
	return err.Error()
}

func (c *apiClient) request(ctx context.Context, method, path string, query url.Values,
	body any) (*http.Request, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(cliSurfaceHeader, "cli")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// redact removes the token from text that may be printed.
func (c *apiClient) redact(text string) string {
	if c.token == "" {
		return text
	}
	return strings.ReplaceAll(text, c.token, cliRedacted)
}

// indentJSON is raw indented for --json output, or raw when it is not JSON.
func indentJSON(raw []byte) string {
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") != nil {
		return string(raw)
	}
	return out.String()
}
