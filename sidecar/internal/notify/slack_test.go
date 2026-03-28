package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSlackSend_FormatMessage(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(
		http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if err := json.Unmarshal(
				body, &receivedBody,
			); err != nil {
				t.Fatalf("unmarshalling body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		}))
	defer srv.Close()

	sender := NewSlackSender()
	ch := Channel{
		ID:     1,
		Name:   "test-slack",
		Type:   "slack",
		Config: map[string]string{"webhook_url": srv.URL},
	}
	evt := Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  "Test action",
		Body:     "Action body",
	}

	err := sender.Send(context.Background(), ch, evt)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}

	blocks, ok := receivedBody["blocks"].([]any)
	if !ok || len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %v",
			receivedBody)
	}

	// Verify header block contains subject
	header := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Errorf("first block type = %v, want header",
			header["type"])
	}
}

func TestSlackSend_BadWebhook(t *testing.T) {
	sender := NewSlackSender()
	ch := Channel{
		ID:     1,
		Name:   "bad",
		Type:   "slack",
		Config: map[string]string{
			"webhook_url": "http://127.0.0.1:1/nope",
		},
	}
	evt := Event{
		Type:     "action_failed",
		Severity: "warning",
		Subject:  "Fail test",
	}

	err := sender.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for bad webhook URL")
	}
}

func TestSlackSend_MissingWebhook(t *testing.T) {
	sender := NewSlackSender()
	ch := Channel{
		ID:     1,
		Name:   "empty",
		Type:   "slack",
		Config: map[string]string{},
	}
	evt := Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  "Test",
	}

	err := sender.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for missing webhook_url")
	}
}

func TestSlackSend_ServerError(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(func(
			w http.ResponseWriter, _ *http.Request,
		) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	defer srv.Close()

	sender := NewSlackSender()
	ch := Channel{
		ID:     1,
		Name:   "err",
		Type:   "slack",
		Config: map[string]string{"webhook_url": srv.URL},
	}
	evt := Event{
		Type:     "action_failed",
		Severity: "critical",
		Subject:  "Err test",
	}

	err := sender.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestBuildSlackPayload_AllSeverities(t *testing.T) {
	sevs := []string{"info", "warning", "critical"}
	for _, sev := range sevs {
		evt := Event{
			Type:     "action_executed",
			Severity: sev,
			Subject:  "Test " + sev,
			Body:     "Body",
		}
		payload, err := buildSlackPayload(evt)
		if err != nil {
			t.Errorf("sev %s: buildSlackPayload: %v",
				sev, err)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Errorf("sev %s: unmarshal: %v", sev, err)
		}
	}
}
