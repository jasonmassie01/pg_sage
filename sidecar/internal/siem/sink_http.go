package siem

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Sink delivers a batch of events. Send returns nil only when the
// receiver accepted the whole batch.
type Sink interface {
	Name() string
	Send(ctx context.Context, events []Event) error
}

// maxResponseBytes bounds what is read of a receiver's reply.
const maxResponseBytes = 64 << 10

type httpSink struct {
	name, url, token string
	client           *http.Client
}

// NewHTTPSink posts each batch as a JSON array of OCSF events, with an
// optional bearer token.
func NewHTTPSink(name, url, token string, timeout time.Duration) Sink {
	return &httpSink{name: name, url: url, token: token,
		client: &http.Client{Timeout: timeout}}
}

func (s *httpSink) Name() string { return s.name }

func (s *httpSink) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("siem sink %s: encode: %w", s.name, err)
	}
	_, err = post(ctx, s.client, s.name, s.url, s.token, body)
	return err
}

// post sends body and returns the reply; any non-2xx status is an error
// naming the status (never the token).
func post(ctx context.Context, client *http.Client, name, url, token string,
	body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("siem sink %s: request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("siem sink %s: deliver: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("siem sink %s: receiver answered HTTP %d", name,
			resp.StatusCode)
	}
	return reply, nil
}
