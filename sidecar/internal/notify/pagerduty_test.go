package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPagerDutySenderType(t *testing.T) {
	s := NewPagerDutySender()
	if s.Type() != "pagerduty" {
		t.Fatalf("expected pagerduty, got %q", s.Type())
	}
}

func TestPagerDutySend_Success(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{
		client: srv.Client(),
		apiURL: srv.URL,
	}

	ch := Channel{
		Name:   "test-pd",
		Type:   "pagerduty",
		Config: map[string]string{"routing_key": "abc123"},
	}
	evt := Event{
		Type:     "finding_critical",
		Severity: "critical",
		Subject:  "High bloat on orders table",
		Body:     "Table bloat exceeds 50%",
		Data:     map[string]any{"database": "production"},
	}

	err := s.Send(context.Background(), ch, evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(received) == 0 {
		t.Fatal("no payload received by server")
	}

	var pd pdEvent
	if err := json.Unmarshal(received, &pd); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if pd.RoutingKey != "abc123" {
		t.Errorf("routing_key = %q, want abc123", pd.RoutingKey)
	}
	if pd.EventAction != "trigger" {
		t.Errorf("event_action = %q, want trigger", pd.EventAction)
	}
	if pd.Payload.Summary != "High bloat on orders table" {
		t.Errorf("summary = %q", pd.Payload.Summary)
	}
	if pd.Payload.Severity != "critical" {
		t.Errorf("severity = %q, want critical", pd.Payload.Severity)
	}
	if pd.Payload.Source != "pg_sage" {
		t.Errorf("source = %q, want pg_sage", pd.Payload.Source)
	}
	if pd.Payload.Component != "production" {
		t.Errorf("component = %q, want production",
			pd.Payload.Component)
	}
	if pd.Payload.Class != "finding_critical" {
		t.Errorf("class = %q, want finding_critical",
			pd.Payload.Class)
	}
}

func TestPagerDutySend_ContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotCT = r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{
		Config: map[string]string{"routing_key": "key"},
	}
	evt := Event{Type: "test", Severity: "info", Subject: "test"}

	_ = s.Send(context.Background(), ch, evt)

	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
}

func TestPagerDutySend_MissingRoutingKey(t *testing.T) {
	s := NewPagerDutySender()
	ch := Channel{
		Name:   "bad",
		Config: map[string]string{},
	}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for missing routing_key")
	}
	if !strings.Contains(err.Error(), "routing_key") {
		t.Errorf("error should mention routing_key: %v", err)
	}
}

func TestPagerDutySend_EmptyRoutingKey(t *testing.T) {
	s := NewPagerDutySender()
	ch := Channel{
		Name:   "bad",
		Config: map[string]string{"routing_key": ""},
	}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for empty routing_key")
	}
}

func TestPagerDutySend_NilConfig(t *testing.T) {
	s := NewPagerDutySender()
	ch := Channel{Name: "bad", Config: nil}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestPagerDutySend_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{"routing_key": "k"}}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code: %v", err)
	}
}

func TestPagerDutySend_BadRequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{"routing_key": "k"}}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain status code: %v", err)
	}
}

func TestPagerDutySend_UnreachableEndpoint(t *testing.T) {
	s := &PagerDutySender{
		client: http.DefaultClient,
		apiURL: "http://127.0.0.1:1",
	}
	ch := Channel{Config: map[string]string{"routing_key": "k"}}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
}

func TestPagerDutySend_AllSeverities(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"critical", "critical"},
		{"warning", "warning"},
		{"info", "info"},
		{"", "info"},
		{"unknown", "info"},
	}

	for _, tt := range tests {
		got := mapPDSeverity(tt.input)
		if got != tt.want {
			t.Errorf("mapPDSeverity(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}

func TestPagerDutySend_CustomSource(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{
		"routing_key": "k",
		"source":      "my-custom-source",
	}}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}

	var pd pdEvent
	json.Unmarshal(received, &pd)
	if pd.Payload.Source != "my-custom-source" {
		t.Errorf("source = %q, want my-custom-source",
			pd.Payload.Source)
	}
}

func TestPagerDutySend_CustomComponent(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{
		"routing_key": "k",
		"component":   "api-gateway",
	}}
	evt := Event{
		Type: "test", Severity: "info", Subject: "x",
		Data: map[string]any{"database": "prod"},
	}

	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}

	var pd pdEvent
	json.Unmarshal(received, &pd)
	if pd.Payload.Component != "api-gateway" {
		t.Errorf("component = %q, want api-gateway (config override)",
			pd.Payload.Component)
	}
}

func TestPagerDutySend_DedupKey(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{"routing_key": "k"}}
	evt := Event{
		Type: "finding_critical", Severity: "critical",
		Subject: "x",
		Data:    map[string]any{"database": "orders"},
	}

	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}

	var pd pdEvent
	json.Unmarshal(received, &pd)
	if pd.DedupKey != "finding_critical:orders" {
		t.Errorf("dedup_key = %q, want finding_critical:orders",
			pd.DedupKey)
	}
}

func TestPagerDutySend_DedupKeyWithPrefix(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{
		"routing_key":      "k",
		"dedup_key_prefix": "env-prod",
	}}
	evt := Event{
		Type: "action_failed", Severity: "warning", Subject: "x",
		Data: map[string]any{"database": "db1"},
	}

	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}

	var pd pdEvent
	json.Unmarshal(received, &pd)
	want := "env-prod:action_failed:db1"
	if pd.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", pd.DedupKey, want)
	}
}

func TestPagerDutySend_DedupKeyNoDatabase(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{"routing_key": "k"}}
	evt := Event{
		Type: "action_executed", Severity: "info", Subject: "x",
	}

	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}

	var pd pdEvent
	json.Unmarshal(received, &pd)
	if pd.DedupKey != "action_executed" {
		t.Errorf("dedup_key = %q, want action_executed",
			pd.DedupKey)
	}
}

func TestBuildPagerDutyPayload_AllEventTypes(t *testing.T) {
	for evtType := range ValidEventTypes {
		ch := Channel{Config: map[string]string{"routing_key": "k"}}
		evt := Event{
			Type: evtType, Severity: "warning",
			Subject: "test " + evtType,
			Data:    map[string]any{"database": "db"},
		}

		data, err := buildPagerDutyPayload(ch, evt, "k")
		if err != nil {
			t.Errorf("event type %q: %v", evtType, err)
			continue
		}

		var pd pdEvent
		if err := json.Unmarshal(data, &pd); err != nil {
			t.Errorf("event type %q: unmarshal: %v", evtType, err)
			continue
		}
		if pd.Payload.Class != evtType {
			t.Errorf("event type %q: class = %q",
				evtType, pd.Payload.Class)
		}
		if pd.Payload.Summary != "test "+evtType {
			t.Errorf("event type %q: summary = %q",
				evtType, pd.Payload.Summary)
		}
	}
}

func TestPagerDutySend_200IsNotSuccess(t *testing.T) {
	// PagerDuty returns 202, not 200. Verify 200 is treated as error.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	defer srv.Close()

	s := &PagerDutySender{client: srv.Client(), apiURL: srv.URL}
	ch := Channel{Config: map[string]string{"routing_key": "k"}}
	evt := Event{Type: "test", Severity: "info", Subject: "x"}

	err := s.Send(context.Background(), ch, evt)
	if err == nil {
		t.Fatal("expected error: PagerDuty should require 202")
	}
	if !strings.Contains(err.Error(), "200") {
		t.Errorf("error should mention 200: %v", err)
	}
}
