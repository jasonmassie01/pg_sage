package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// alerting-06 M4: the notify webhook sender replaces the YAML stack's
// webhook channel. Its URL and headers are channel secrets.
// No concurrent access tests: the sender is stateless apart from its
// http.Client, which is safe for concurrent use.

type webhookCapture struct {
	body    map[string]any
	headers http.Header
}

func webhookServer(t *testing.T, status int, got *webhookCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		if got != nil {
			got.headers = r.Header.Clone()
			if err := json.Unmarshal(raw, &got.body); err != nil {
				t.Errorf("webhook body is not JSON: %v (%s)", err, raw)
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func webhookChannel(url, headers string) Channel {
	cfg := map[string]string{"webhook_url": url}
	if headers != "" {
		cfg["headers"] = headers
	}
	return Channel{ID: 4, Name: "ops-hook", Type: "webhook", Config: cfg}
}

func TestWebhookSend_PostsEventJSONWithHeaders(t *testing.T) {
	var got webhookCapture
	srv := webhookServer(t, http.StatusNoContent, &got)
	s := NewWebhookSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	evt := Event{Type: "finding_opened", Severity: "warning", Subject: "Bloat",
		Body: "table t", DedupKey: "db1:bloat:t",
		Data: map[string]any{"database": "db1", "category": "bloat"}}
	ch := webhookChannel(srv.URL, `{"X-Token":"s3cret","X-Team":"dba"}`)
	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if s.Type() != "webhook" {
		t.Fatalf("Type = %q, want webhook", s.Type())
	}
	want := map[string]any{"event": "finding_opened", "severity": "warning",
		"subject": "Bloat", "body": "table t", "dedup_key": "db1:bloat:t",
		"database": "db1", "resolve": false}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("payload[%s] = %#v, want %#v", k, got.body[k], v)
		}
	}
	data, _ := got.body["data"].(map[string]any)
	if data["category"] != "bloat" {
		t.Errorf("payload data = %#v, want category bloat", got.body["data"])
	}
	if got.headers.Get("X-Token") != "s3cret" || got.headers.Get("X-Team") != "dba" {
		t.Errorf("headers = %v, want X-Token and X-Team", got.headers)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestWebhookSend_ResolveCarriesFlag(t *testing.T) {
	var got webhookCapture
	srv := webhookServer(t, http.StatusOK, &got)
	s := NewWebhookSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	evt := Event{Type: "finding_resolved", Severity: "critical", Subject: "x",
		DedupKey: "k", Resolve: true}
	if err := s.Send(context.Background(), webhookChannel(srv.URL, ""), evt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.body["resolve"] != true {
		t.Fatalf("resolve = %#v, want true", got.body["resolve"])
	}
}

func TestWebhookSend_StatusErrorIsTyped(t *testing.T) {
	srv := webhookServer(t, http.StatusServiceUnavailable, nil)
	s := NewWebhookSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	err := s.Send(context.Background(), webhookChannel(srv.URL, ""), Event{Type: "x"})
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *HTTPStatusError 503", err)
	}
	if !Retryable(err) {
		t.Fatalf("503 must be retryable")
	}
}

func TestWebhookSend_InvalidConfig(t *testing.T) {
	s := NewWebhookSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	cases := map[string]Channel{
		"missing url":      {Name: "a", Type: "webhook", Config: map[string]string{}},
		"nil config":       {Name: "a", Type: "webhook"},
		"headers not json": webhookChannel("http://127.0.0.1:1/x", "{not json"),
		"headers array":    webhookChannel("http://127.0.0.1:1/x", `["a"]`),
		"bad header name":  webhookChannel("http://127.0.0.1:1/x", `{"Bad Name":"v"}`),
		"host header":      webhookChannel("http://127.0.0.1:1/x", `{"Host":"evil"}`),
	}
	for name, ch := range cases {
		err := s.Send(context.Background(), ch, Event{Type: "x"})
		if err == nil || !strings.Contains(err.Error(), "webhook channel") {
			t.Errorf("%s: err = %v, want a webhook channel config error", name, err)
		}
	}
}

func TestWebhookSend_StrictPolicyRefusesLoopback(t *testing.T) {
	srv := webhookServer(t, http.StatusOK, nil)
	err := NewWebhookSender().Send(context.Background(),
		webhookChannel(srv.URL, ""), Event{Type: "x"})
	if err == nil {
		t.Fatal("strict policy delivered to a loopback webhook")
	}
	if Retryable(err) {
		t.Fatalf("a refused target must not be retried: %v", err)
	}
}

func TestWebhookSend_ErrorRedactsURLSecret(t *testing.T) {
	s := NewWebhookSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := webhookChannel("http://127.0.0.1:1/hooks/T0KENSECRET", "")
	err := s.Send(context.Background(), ch, Event{Type: "x"})
	if err == nil {
		t.Fatal("send to a closed port succeeded")
	}
	if strings.Contains(err.Error(), "T0KENSECRET") {
		t.Fatalf("error leaks the URL secret: %v", err)
	}
}

func TestWebhookHeaders_AreChannelSecrets(t *testing.T) {
	found := false
	for _, k := range SecretConfigKeys {
		if k == "headers" {
			found = true
		}
	}
	if !found {
		t.Fatalf("headers is not a secret config key: %v", SecretConfigKeys)
	}
}
