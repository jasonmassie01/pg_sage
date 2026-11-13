package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// postJSON posts body to url as JSON with headers. A non-2xx answer is an
// *HTTPStatusError that carries the endpoint's Retry-After hint, so the
// outbox waits as long as the destination asked (market-14).
func postJSON(
	ctx context.Context, client *http.Client, service, url string, body []byte,
	headers map[string]string,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create %s request: %w", service, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s http post: %w", service, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPStatusError{Service: service, Code: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	return nil
}

// maxRetryAfterSeconds keeps a huge hint from overflowing a Duration.
const maxRetryAfterSeconds = 7 * 24 * 3600

// parseRetryAfter reads a Retry-After value: delay seconds or an HTTP
// date. A missing, malformed, negative or past value is 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		secs = min(secs, maxRetryAfterSeconds)
		return time.Duration(secs) * time.Second
	}
	at, err := http.ParseTime(v)
	if err != nil || !at.After(now) {
		return 0
	}
	return at.Sub(now).Round(time.Second)
}
