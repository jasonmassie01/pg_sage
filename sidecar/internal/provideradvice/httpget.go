package provideradvice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// maxResponseBytes bounds one provider response.
const maxResponseBytes = 4 << 20

// newHTTPClient is the egress-policy client with redirects refused: a
// bearer token is never replayed to another location.
func newHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = egress.HTTPClient(20 * time.Second)
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

// checkEndpoint accepts an https base URL (http only for loopback tests).
func checkEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return "", fmt.Errorf("endpoint %q must be an https URL", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

func loopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// getJSON sends an authorized GET and returns a bounded 200 body. api names
// the call in errors; the token never appears in one.
func getJSON(ctx context.Context, client *http.Client, rawURL, token, api string,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build %s request", ErrProvider, api)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s unreachable; check network access", ErrProvider, api)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: %s response unreadable or larger than %d bytes",
			ErrMalformed, api, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(api, resp.StatusCode, raw)
	}
	return raw, nil
}

func statusError(api string, status int, raw []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e) // the body is optional detail
	detail := strings.TrimSpace(clip(e.Error.Code) + " " + clip(e.Error.Message+e.Message))
	msg := fmt.Sprintf("%s HTTP %d %s", api, status, detail)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s; check the credential and its read role", ErrAuth,
			strings.TrimSpace(msg))
	}
	return fmt.Errorf("%w: %s", ErrProvider, strings.TrimSpace(msg))
}

// clip keeps provider text short and on one line.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
