package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// mockDispatcher records dispatched events for testing.
type mockDispatcher struct {
	events []notify.Event
	err    error // if set, Dispatch returns this error
}

func (m *mockDispatcher) Dispatch(
	_ context.Context, event notify.Event,
) error {
	m.events = append(m.events, event)
	return m.err
}

func TestDispatchEvent_NilDispatcher(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		dispatcher:    nil,
	}

	// Must not panic with nil dispatcher.
	e.dispatchEvent(context.Background(),
		notify.ActionExecutedEvent("test", "SELECT 1", "mydb"))
}

func TestDispatchEvent_Success(t *testing.T) {
	d := &mockDispatcher{}
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		dispatcher:    d,
		databaseName:  "testdb",
	}

	event := notify.ActionExecutedEvent(
		"test action", "SELECT 1", "testdb")
	e.dispatchEvent(context.Background(), event)

	if len(d.events) != 1 {
		t.Fatalf("got %d events, want 1", len(d.events))
	}
	if d.events[0].Type != "action_executed" {
		t.Errorf("type = %q, want action_executed",
			d.events[0].Type)
	}
}

func TestDispatchEvent_ErrorLogged(t *testing.T) {
	var logged bool
	d := &mockDispatcher{err: errors.New("send failed")}
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn: func(_, msg string, args ...any) {
			logged = true
		},
		dispatcher:   d,
		databaseName: "testdb",
	}

	e.dispatchEvent(context.Background(),
		notify.ActionFailedEvent(
			"test", "SELECT 1", "testdb", "oops"))

	if !logged {
		t.Error("expected error to be logged")
	}
	if len(d.events) != 1 {
		t.Fatalf("got %d events, want 1", len(d.events))
	}
}

func TestWithDispatcher_SetsField(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
	}

	d := &mockDispatcher{}
	e.WithDispatcher(d)

	if e.dispatcher == nil {
		t.Error("dispatcher should be set")
	}
}

func TestWithDatabaseName_SetsField(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
	}

	e.WithDatabaseName("mydb")

	if e.databaseName != "mydb" {
		t.Errorf("databaseName = %q, want mydb",
			e.databaseName)
	}
}

func TestDispatchEvent_AllEventTypes(t *testing.T) {
	d := &mockDispatcher{}
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		dispatcher:    d,
		databaseName:  "testdb",
	}

	ctx := context.Background()

	e.dispatchEvent(ctx,
		notify.ActionExecutedEvent("a", "sql", "testdb"))
	e.dispatchEvent(ctx,
		notify.ActionFailedEvent("b", "sql", "testdb", "err"))
	e.dispatchEvent(ctx,
		notify.ApprovalNeededEvent("c", "sql", "testdb", "high"))

	if len(d.events) != 3 {
		t.Fatalf("got %d events, want 3", len(d.events))
	}

	want := []string{
		"action_executed", "action_failed", "approval_needed",
	}
	for i, w := range want {
		if d.events[i].Type != w {
			t.Errorf("event[%d].Type = %q, want %q",
				i, d.events[i].Type, w)
		}
	}
}
