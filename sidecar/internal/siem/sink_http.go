package siem

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

// ErrPermanent marks a refusal of the events themselves, which the receiver
// repeats however often they are sent: HTTP 400, 413 or 422, OTLP rejected
// log records, an event too large for syslog, an event that cannot be
// encoded. Every other failure (network, auth, 429, 5xx) is transient.
var ErrPermanent = errors.New("the receiver refuses these events")

// permanentStatus reports whether an HTTP status refuses the request's
// content rather than the moment it was sent.
func permanentStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}

type httpSink struct {
	name, url, token string
	client           *http.Client
}

// NewHTTPSink posts each batch as a JSON array of OCSF events, with an
// optional bearer token. tlsCfg (LoadTLSConfig) sets the receiver's roots
// and the client pair; nil keeps the default transport.
func NewHTTPSink(name, url, token string, timeout time.Duration, tlsCfg *tls.Config) Sink {
	return &httpSink{name: name, url: url, token: token,
		client: httpClient(timeout, tlsCfg)}
}

func (s *httpSink) Name() string { return s.name }

func (s *httpSink) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("siem sink %s: encode: %w: %w", s.name, ErrPermanent, err)
	}
	_, err = post(ctx, s.client, s.name, s.url, s.token, body)
	return err
}

// post sends body and returns the reply; any non-2xx status is an error
// naming the status (never the token), ErrPermanent when it refuses the
// content.
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
	if permanentStatus(resp.StatusCode) {
		return nil, fmt.Errorf("siem sink %s: receiver answered HTTP %d: %w", name,
			resp.StatusCode, ErrPermanent)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("siem sink %s: receiver answered HTTP %d", name,
			resp.StatusCode)
	}
	return reply, nil
}
