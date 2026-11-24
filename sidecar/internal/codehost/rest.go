package codehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// maxResponseBytes bounds a response body: a 1 MiB file base64-encoded in
// a JSON document, with room to spare.
const maxResponseBytes = 2 * MaxFileBytes

// maxErrorSnippet bounds the host's message quoted in an error.
const maxErrorSnippet = 200

// rest is the HTTP core shared by the GitHub and GitLab clients.
type rest struct {
	kind    string
	base    *url.URL
	token   string
	auth    func(req *http.Request, token string)
	client  *http.Client
	timeout time.Duration
	sem     chan struct{}
	now     func() time.Time

	mu           sync.Mutex
	blockedUntil time.Time
}

// call is one request: path is already escaped and relative to the base.
type call struct {
	method string
	path   string
	query  url.Values
	body   any
	repo   string
	write  bool
}

func newRest(kind, defaultBase string, opts Options,
	auth func(*http.Request, string)) (*rest, error) {
	raw := opts.BaseURL
	if raw == "" {
		raw = defaultBase
	}
	base, err := parseBase(raw)
	if err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrent
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &rest{kind: kind, base: base, token: opts.Token, auth: auth,
		client: sameOriginClient(base), timeout: opts.Timeout,
		sem: make(chan struct{}, opts.MaxConcurrent), now: opts.Now}, nil
}

// sameOriginClient is the egress client that follows a redirect only
// within base's origin. net/http forwards custom headers such as GitLab's
// PRIVATE-TOKEN to any redirect target, so a redirect elsewhere is refused
// rather than handing the token to another host.
func sameOriginClient(base *url.URL) *http.Client {
	client := egress.HTTPClient(0)
	floor := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := floor(req, via); err != nil {
			return err
		}
		if !egress.SameOrigin(req.URL.String(), base.String()) {
			return fmt.Errorf("redirect to another origin %s://%s refused: the token is "+
				"sent only to %s", req.URL.Scheme, req.URL.Host, base.Host)
		}
		return nil
	}
	return client
}

// parseBase checks the API root: https (http only on loopback), no user
// information, query or fragment, and above the egress floor.
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: code host URL %q is not an absolute URL", ErrInvalid, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: code host URL must not carry credentials, a query "+
			"or a fragment", ErrInvalid)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("%w: code host URL %s must use https", ErrInvalid, u.Host)
	}
	if err := egress.CheckURL(u); err != nil {
		return nil, fmt.Errorf("%w: code host URL %s: %v", ErrInvalid, u.Host, err)
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (r *rest) name() string { return r.kind + " " + r.base.Host }

// getJSON runs c and decodes a JSON answer into out.
func (r *rest) getJSON(ctx context.Context, c call, out any) error {
	body, err := r.do(ctx, c)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: codehost %s: %s %s: unexpected answer: %v",
			ErrUnavailable, r.name(), c.method, c.path, err)
	}
	return nil
}

// do sends c under the rate-limit cool-down, the concurrency cap and the
// request timeout, and maps the answer's status to the package errors.
func (r *rest) do(ctx context.Context, c call) ([]byte, error) {
	if err := r.coolingDown(); err != nil {
		return nil, err
	}
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("codehost %s: %s %s: waiting for a request slot: %w",
			r.name(), c.method, c.path, ctx.Err())
	}
	defer func() { <-r.sem }()
	// A rate-limit answer may have arrived while this request waited.
	if err := r.coolingDown(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := r.request(ctx, c)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, r.transportError(ctx, c, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, r.transportError(ctx, c, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: codehost %s: %s %s: answer larger than %d bytes",
			ErrTooLarge, r.name(), c.method, c.path, maxResponseBytes)
	}
	if resp.StatusCode/100 == 2 {
		return body, nil
	}
	return nil, r.statusError(c, resp, body)
}

func (r *rest) request(ctx context.Context, c call) (*http.Request, error) {
	var body io.Reader
	if c.body != nil {
		raw, err := json.Marshal(c.body)
		if err != nil {
			return nil, fmt.Errorf("%w: encode request body: %v", ErrInvalid, err)
		}
		body = bytes.NewReader(raw)
	}
	u := *r.base
	unescaped, err := url.PathUnescape(c.path)
	if err != nil {
		return nil, fmt.Errorf("%w: request path: %v", ErrInvalid, err)
	}
	u.Path = r.base.Path + unescaped
	u.RawPath = r.base.EscapedPath() + c.path
	u.RawQuery = c.query.Encode()
	req, err := http.NewRequestWithContext(ctx, c.method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrInvalid, err)
	}
	req.Header.Set("User-Agent", "pg_sage")
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.token != "" {
		r.auth(req, r.token)
	}
	return req, nil
}

// transportError names the host and keeps the context and egress errors
// distinguishable. The url.Error wrapper is dropped: its text repeats the
// whole URL.
func (r *rest) transportError(ctx context.Context, c call, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("codehost %s: %s %s: request did not finish: %w",
			r.name(), c.method, c.path, ctxErr)
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	if errors.Is(err, egress.ErrBlocked) {
		return fmt.Errorf("codehost %s: %w", r.name(), err)
	}
	return fmt.Errorf("%w: codehost %s: %s %s: %v", ErrUnavailable, r.name(),
		c.method, c.path, r.redact(err.Error()))
}

// statusError maps a non-2xx answer.
func (r *rest) statusError(c call, resp *http.Response, body []byte) error {
	if limited(resp) {
		return r.startCoolDown(resp.Header)
	}
	where := fmt.Sprintf("codehost %s: %s %s", r.name(), c.method, c.path)
	msg := r.snippet(body)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		verb := "read"
		if c.write {
			verb = "write to"
		}
		return fmt.Errorf("%w: token cannot %s %s (HTTP %d) [%s]: %s", ErrForbidden, verb,
			c.repo, resp.StatusCode, where, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s: HTTP 404 in %s: %s", ErrNotFound, where, c.repo, msg)
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s: HTTP %d: %s", ErrInvalid, where, resp.StatusCode, msg)
	}
	return fmt.Errorf("%w: %s: HTTP %d: %s", ErrUnavailable, where, resp.StatusCode, msg)
}

// snippet is the host's own message, bounded, without control characters
// and with the token redacted.
func (r *rest) snippet(body []byte) string {
	var doc struct {
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	text := string(body)
	if json.Unmarshal(body, &doc) == nil {
		switch {
		case doc.Message != nil:
			text = fmt.Sprint(doc.Message)
		case doc.Error != nil:
			text = fmt.Sprint(doc.Error)
		}
	}
	text = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return ' '
		}
		return c
	}, r.redact(text))
	if len(text) > maxErrorSnippet {
		text = text[:maxErrorSnippet] + "..."
	}
	return strings.TrimSpace(text)
}

// redact removes the token from text a host or transport produced.
func (r *rest) redact(text string) string {
	if r.token == "" {
		return text
	}
	return strings.ReplaceAll(text, r.token, "[redacted]")
}
