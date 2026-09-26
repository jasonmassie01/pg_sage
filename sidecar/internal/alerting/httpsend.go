package alerting

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// statusError is a non-2xx HTTP response.
type statusError struct {
	service string
	code    int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s returned status %d", e.service, e.code)
}

// retryable reports whether another attempt can succeed: network
// errors, 5xx and 429. Other 4xx responses are permanent (G7-B32).
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return true
}

// postJSON posts payload and returns a redacted error: webhook URLs
// carry their secret in the path, which *url.Error would echo (G7-B10).
func postJSON(
	ctx context.Context, client *http.Client, service, url string,
	payload []byte, headers map[string]string,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create %s request: %w", service, notify.RedactError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s http post: %w", service, notify.RedactError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{service: service, code: resp.StatusCode}
	}
	return nil
}

// sendWithRetry makes up to 3 attempts with exponential backoff,
// stopping early on a permanent error or context cancellation.
func sendWithRetry(
	ctx context.Context, service string,
	logFn func(string, string, ...any), attempt func() error,
) error {
	const maxAttempts = 3
	backoff := time.Second
	var lastErr error
	for i := range maxAttempts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s send cancelled: %w", service, err)
		}
		lastErr = attempt()
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			return fmt.Errorf("%s send failed permanently: %w", service, lastErr)
		}
		if i < maxAttempts-1 {
			logFn("WARN", "%s retry %d/%d: %v", service, i+1, maxAttempts, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("%s send cancelled: %w", service, ctx.Err())
			}
			backoff *= 2
		}
	}
	return fmt.Errorf("%s send failed after %d attempts: %w",
		service, maxAttempts, lastErr)
}
