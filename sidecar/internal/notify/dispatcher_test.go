package notify

import (
	"context"
	"sync"
	"testing"
)

// mockSender records calls for testing.
type mockSender struct {
	mu       sync.Mutex
	typ      string
	calls    []mockCall
	failNext bool
}

type mockCall struct {
	Channel Channel
	Event   Event
}

func newMockSender(typ string) *mockSender {
	return &mockSender{typ: typ}
}

func (m *mockSender) Type() string { return m.typ }

func (m *mockSender) Send(
	_ context.Context, ch Channel, evt Event,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, mockCall{ch, evt})
	if m.failNext {
		m.failNext = false
		return errSendFailed
	}
	return nil
}

func (m *mockSender) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

var errSendFailed = errorf("mock send failed")

type errString string

func errorf(s string) errString { return errString(s) }
func (e errString) Error() string { return string(e) }

func TestSeverityMeetsMin(t *testing.T) {
	tests := []struct {
		sev, min string
		want     bool
	}{
		{"critical", "info", true},
		{"critical", "warning", true},
		{"critical", "critical", true},
		{"warning", "info", true},
		{"warning", "warning", true},
		{"warning", "critical", false},
		{"info", "info", true},
		{"info", "warning", false},
		{"info", "critical", false},
	}
	for _, tt := range tests {
		got := SeverityMeetsMin(tt.sev, tt.min)
		if got != tt.want {
			t.Errorf(
				"SeverityMeetsMin(%q, %q) = %v, want %v",
				tt.sev, tt.min, got, tt.want)
		}
	}
}

func TestValidEventTypes(t *testing.T) {
	expected := []string{
		"action_executed", "action_failed",
		"approval_needed", "finding_critical",
	}
	for _, e := range expected {
		if !ValidEventTypes[e] {
			t.Errorf("expected %q to be valid event type", e)
		}
	}
	if ValidEventTypes["bogus"] {
		t.Error("expected 'bogus' to be invalid event type")
	}
}
