package alerting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mockChannel records alerts for testing.
type mockChannel struct {
	mu     sync.Mutex
	name   string
	alerts []Alert
	err    error
}

func (m *mockChannel) Name() string { return m.name }

func (m *mockChannel) Send(
	_ context.Context, a Alert,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = append(m.alerts, a)
	return m.err
}

func (m *mockChannel) alertCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.alerts)
}

func noopLog(string, string, ...any) {}

// --- Throttle tests ---

func TestThrottle_ShouldAlert_FirstTime(t *testing.T) {
	th := NewThrottle(5, "", "", "")
	if !th.ShouldAlert("key1", "warning") {
		t.Fatal("first alert for a key should pass")
	}
}

func TestThrottle_ShouldAlert_Cooldown(t *testing.T) {
	th := NewThrottle(60, "", "", "")
	th.Record("key1", "warning")
	if th.ShouldAlert("key1", "warning") {
		t.Fatal("same key within cooldown should be blocked")
	}
}

func TestThrottle_ShouldAlert_Escalation(t *testing.T) {
	th := NewThrottle(60, "", "", "")
	th.Record("key1", "warning")
	if !th.ShouldAlert("key1", "critical") {
		t.Fatal(
			"escalation from warning to critical " +
				"should bypass cooldown",
		)
	}
}

func TestThrottle_QuietHours(t *testing.T) {
	th := NewThrottle(5, "22:00", "06:00", "UTC")

	// 23:00 UTC is within quiet hours.
	qh := time.Date(2026, 3, 27, 23, 0, 0, 0, time.UTC)
	if !th.IsQuietHours(qh) {
		t.Fatal("23:00 should be in quiet hours 22-06")
	}

	// 12:00 UTC is outside quiet hours.
	day := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	if th.IsQuietHours(day) {
		t.Fatal("12:00 should not be in quiet hours 22-06")
	}
}

func TestThrottle_QuietHours_Disabled(t *testing.T) {
	th := NewThrottle(5, "", "", "")

	any := time.Date(2026, 3, 27, 3, 0, 0, 0, time.UTC)
	if th.IsQuietHours(any) {
		t.Fatal("empty quiet hours should never block")
	}
}

func TestThrottle_QuietHours_SameDay(t *testing.T) {
	th := NewThrottle(5, "02:00", "06:00", "UTC")

	inside := time.Date(2026, 3, 27, 3, 0, 0, 0, time.UTC)
	if !th.IsQuietHours(inside) {
		t.Fatal("03:00 should be in quiet hours 02-06")
	}

	outside := time.Date(2026, 3, 27, 8, 0, 0, 0, time.UTC)
	if th.IsQuietHours(outside) {
		t.Fatal("08:00 should not be in quiet hours 02-06")
	}
}

func TestThrottle_Reset(t *testing.T) {
	th := NewThrottle(60, "", "", "")
	th.Record("key1", "warning")
	th.Reset()
	if !th.ShouldAlert("key1", "warning") {
		t.Fatal("after reset, key should be allowed again")
	}
}

// --- Slack tests ---

func TestSlackPayload_Format(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			received = body
			w.WriteHeader(http.StatusOK)
		},
	))
	defer srv.Close()

	ch := NewSlack(srv.URL, noopLog)
	alert := Alert{
		Findings: []AlertFinding{
			{
				ID: 1, Title: "Unused index",
				Category:         "index_unused",
				Severity:         "warning",
				ObjectType:       "index",
				ObjectIdentifier: "idx_foo",
				OccurrenceCount:  3,
				Recommendation:   "Drop the index",
			},
		},
		Severity:  "warning",
		Timestamp: time.Now(),
	}

	err := ch.Send(context.Background(), alert)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	blocks, ok := payload["blocks"].([]any)
	if !ok {
		t.Fatal("expected blocks array")
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	header := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Fatalf("first block should be header, got %s",
			header["type"])
	}
}

// --- PagerDuty tests ---

func TestPagerDutyPayload_Format(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			received = body
			w.WriteHeader(http.StatusAccepted)
		},
	))
	defer srv.Close()

	// Override the PD URL for testing.
	ch := &PagerDutyChannel{
		routingKey: "test-key",
		client:     &http.Client{Timeout: 5 * time.Second},
		logFn:      noopLog,
	}

	alert := Alert{
		Findings: []AlertFinding{
			{
				ID:               1,
				Title:            "Sequence near exhaustion",
				Category:         "sequence_exhaustion",
				Severity:         "critical",
				ObjectIdentifier: "users_id_seq",
			},
		},
		Severity:  "critical",
		Timestamp: time.Now(),
	}

	// Build payload and post to test server instead.
	payload, err := ch.buildPayload(alert)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}

	resp, err := http.Post(
		srv.URL, "application/json",
		io.NopCloser(
			io.NewSectionReader(
				readerAt(payload), 0, int64(len(payload)),
			),
		),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	var ev map[string]any
	if err := json.Unmarshal(received, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if ev["routing_key"] != "test-key" {
		t.Fatalf("expected routing_key=test-key, got %v",
			ev["routing_key"])
	}
	if ev["event_action"] != "trigger" {
		t.Fatalf("expected event_action=trigger, got %v",
			ev["event_action"])
	}
	if ev["dedup_key"] != "sequence_exhaustion:users_id_seq" {
		t.Fatalf("unexpected dedup_key: %v", ev["dedup_key"])
	}

	pd, ok := ev["payload"].(map[string]any)
	if !ok {
		t.Fatal("expected payload object")
	}
	if pd["severity"] != "critical" {
		t.Fatalf("expected severity=critical, got %v",
			pd["severity"])
	}
}

// readerAt wraps a byte slice as io.ReaderAt.
type readerAt []byte

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r)) {
		return 0, io.EOF
	}
	n := copy(p, r[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// --- Webhook tests ---

func TestWebhook_CustomHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header
			w.WriteHeader(http.StatusOK)
		},
	))
	defer srv.Close()

	headers := map[string]string{
		"X-Api-Key":    "secret123",
		"X-Custom-Tag": "pg_sage",
	}
	ch := NewWebhook("myapp", srv.URL, headers, noopLog)

	alert := Alert{
		Findings:  []AlertFinding{{ID: 1, Title: "Test"}},
		Severity:  "info",
		Timestamp: time.Now(),
	}

	err := ch.Send(context.Background(), alert)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if ch.Name() != "webhook:myapp" {
		t.Fatalf("expected name webhook:myapp, got %s",
			ch.Name())
	}

	if gotHeaders.Get("X-Api-Key") != "secret123" {
		t.Fatalf("expected X-Api-Key=secret123, got %s",
			gotHeaders.Get("X-Api-Key"))
	}
	if gotHeaders.Get("X-Custom-Tag") != "pg_sage" {
		t.Fatalf("expected X-Custom-Tag=pg_sage, got %s",
			gotHeaders.Get("X-Custom-Tag"))
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Fatal("expected Content-Type application/json")
	}
}

// --- Manager tests ---

func TestManager_RoutesBySeverity(t *testing.T) {
	pd := &mockChannel{name: "pagerduty"}
	slack := &mockChannel{name: "slack"}

	routes := map[string][]Channel{
		"critical": {pd, slack},
		"warning":  {slack},
	}

	m := New(nil, ManagerConfig{CooldownMinutes: 1}, routes,
		noopLog)

	ctx := context.Background()

	critAlert := Alert{
		Findings:  []AlertFinding{{ID: 1, Title: "Crit"}},
		Severity:  "critical",
		Timestamp: time.Now(),
	}
	m.dispatch(ctx, routes["critical"], critAlert, 1, "k1")

	warnAlert := Alert{
		Findings:  []AlertFinding{{ID: 2, Title: "Warn"}},
		Severity:  "warning",
		Timestamp: time.Now(),
	}
	m.dispatch(ctx, routes["warning"], warnAlert, 2, "k2")

	if pd.alertCount() != 1 {
		t.Fatalf("PD should have 1 alert, got %d",
			pd.alertCount())
	}
	if slack.alertCount() != 2 {
		t.Fatalf("Slack should have 2 alerts, got %d",
			slack.alertCount())
	}
}

func TestManager_DedupKey(t *testing.T) {
	key := FormatDedupKey("index_unused", "idx_users_email")
	expected := "index_unused:idx_users_email"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func TestSeverityEmoji(t *testing.T) {
	cases := []struct {
		sev  string
		want string
	}{
		{"critical", "\xf0\x9f\x94\xb4"},
		{"warning", "\xe2\x9a\xa0\xef\xb8\x8f"},
		{"info", "\xe2\x84\xb9\xef\xb8\x8f"},
	}
	for _, tc := range cases {
		got := severityEmoji(tc.sev)
		if got != tc.want {
			t.Errorf("severityEmoji(%q) = %q, want %q",
				tc.sev, got, tc.want)
		}
	}
}
